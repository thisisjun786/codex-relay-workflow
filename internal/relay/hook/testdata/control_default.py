"""Replay a real owner's receipt store through Python CLI and Go control.sock.

No timestamp/path normalizers: both evaluations use the same marker and store,
with only Python's guard-created files removed before the Go evaluation.
"""
import contextlib
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys

from codex_session_relay import intent, manifest, marker
from codex_session_relay.clock import FakeClock
from codex_session_relay.identity import event_id
from codex_session_relay.models import Endpoint, TurnRef
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store, resolve_state_dir

NOW = '2026-01-01T00:06:00+00:00'


def files(root: Path) -> dict[str, dict[str, str | int]]:
    return {str(p.relative_to(root)): {'bytes': p.read_bytes().hex(), 'mode': p.stat().st_mode & 0o777}
            for p in root.rglob('*') if p.is_file()}


def databases(home: Path) -> dict[str, list[str]]:
    result = {}
    for db in home.rglob('*.sqlite3'):
        with contextlib.closing(sqlite3.connect(db)) as conn:
            result[str(db.relative_to(home))] = list(conn.iterdump())
    return result


def prepare(home: Path, case: str) -> None:
    os.environ.update(HOME=str(home), CODEX_HOME=str(home),
                      XDG_STATE_HOME=str(home / 'xdg'), CODEX_SESSION_RELAY_STATE='')
    state = resolve_state_dir().path
    owner_db = state / 'relay.sqlite3'
    work, root = home / 'work', home / 'markers'
    work.mkdir()
    pinned = case in ('intent_pin', 'request_pin')
    receipt_db = home / 'pinned/relay.sqlite3' if pinned else owner_db
    if pinned:
        with contextlib.closing(Store(str(owner_db))):
            pass
    clock = FakeClock()
    assignment = marker.assignment_id('dispatch')
    directory = marker.assignment_dir(root, work, assignment)
    with contextlib.closing(Store(str(receipt_db))) as store:
        registry = Registry(store, clock)
        relation = registry.register(parent=Endpoint('parent', 'host'),
                                     child=Endpoint('child', 'host', cwd=str(work)),
                                     issue_key='T-1', artifact_roots=[str(work)],
                                     allowed_recipients=['parent'], dispatch_request_id='dispatch',
                                     dispatch_turn_id='turn')
        rid = relation['relationshipId']
        # No dbPath in either settings or intent for the two fallback cases.
        intent_db = str(receipt_db) if case == 'intent_pin' else str(owner_db) if case == 'request_pin' else None
        intent.declare_intent(root, workspace=work, dispatch_request_id='dispatch', issue_key='T-1',
                              declared_at=NOW, db_path=intent_db)
        intent.publish_claim(root, workspace=work, assignment=assignment, session_id='child',
                             dispatch_request_id='dispatch', first_turn_id='turn', at=NOW)
        intent.bind(root, workspace=work, assignment=assignment, session_id='child', task_id='child', at=NOW)
        intent.register_relationship(root, workspace=work, assignment=assignment, relationship_id=rid,
                                     dispatch_request_id='dispatch', db_path=str(receipt_db), at=NOW)
        intent.publish_disposition(root, workspace=work, assignment=assignment, session_id='child',
                                   turn_id='turn', outcome='ready_for_review', at=NOW)
        if case != 'default_missing_receipt':
            artifact = work / 'out.txt'
            artifact.write_text('reviewable work')
            entries, _ = manifest.build([str(artifact)], [str(work)])
            revision = manifest.revision_hash(entries)
            turn = TurnRef('child', 'turn', 'completed')
            payload = dict(eventId=event_id(rid, 1, revision, 'ready_for_review', turn_id='turn', attempt=1),
                           relationshipId=rid, executionGeneration=1, attempt=1, revisionHash=revision,
                           outcome='ready_for_review', producer='child', turnRef=turn.to_record(),
                           manifest=[e.to_record() for e in entries], emittedAt=clock.iso())
            ReceiptIntake(store, registry, clock).accept_child_receipt(payload, observation=turn)
    stop = dict(cwd=str(work), session_id='child', turn_id='turn', stop_hook_active=False)
    config = dict(configVersion=1, mode='hold', isolationAssertedBy='fixture',
                  relayExecutable=str(home / 'unused'), markerRoot=str(root), timeoutSeconds=5,
                  journalRoot=str(home / 'journal'))
    request_db = str(receipt_db) if case == 'request_pin' else None
    if request_db:
        config['dbPath'] = request_db
    (home / 'crw-completion-hook.json').write_text(json.dumps(config))
    before, stores = files(root), databases(home)
    # No --state or --db-path in the fallback cases: the actual CLI discovers the
    # owner's database using XDG, just as an unpinned adapter subprocess does.
    args = [sys.executable, '-m', 'codex_session_relay.cli', 'guard-evaluate',
            '--marker-root', str(root), '--mode', 'hold', '--now', NOW]
    if request_db:
        args.extend(['--db-path', request_db])
    cli = subprocess.run(args, input=json.dumps(stop).encode(), capture_output=True, timeout=15)
    assert cli.returncode == 0 and cli.stderr == b'', cli
    verdict = json.loads(cli.stdout)
    assert verdict['decision'] == ('block' if case == 'default_missing_receipt' else 'release'), verdict
    assert verdict['state'] == ('receipt_missing' if case == 'default_missing_receipt' else 'declared_ready_receipted'), verdict
    (home / 'python.stdout').write_bytes(cli.stdout)
    (home / 'python.files.json').write_text(json.dumps(files(root)))
    (home / 'stores.json').write_text(json.dumps(stores))
    assert databases(home) == stores, 'Python guard changed the relay store'
    after = files(root)
    for name, value in before.items():
        assert after[name] == value, ('changed input marker', name)
    for name in after.keys() - before.keys():
        (root / name).unlink()
    (home / 'request.json').write_text(json.dumps(dict(protocol=1, method='guard-evaluate',
        params=dict(markerRoot=str(root), stopInput=stop, mode='hold', dbPath=request_db, now=NOW, noRecord=False))))
    print(json.dumps(dict(state=str(state), decision=verdict['decision'], guardState=verdict['state'])))


def compare(home: Path) -> None:
    expected = (home / 'python.stdout').read_bytes()
    verdict = json.loads(expected)
    wire = (home / 'go.response').read_bytes()
    assert wire == (json.dumps(verdict, separators=(',', ':')) + '\n').encode(), (expected, wire)
    # Also reconstruct the CLI framing and compare the complete ordered envelope.
    assert (json.dumps(json.loads(wire), indent=2) + '\n').encode() == expected
    actual = files(home / 'markers')
    assert actual == json.loads((home / 'python.files.json').read_text()), actual
    assert databases(home) == json.loads((home / 'stores.json').read_text()), 'Go guard changed a store'
    print(json.dumps(dict(equal=True, decision=verdict['decision'], state=verdict['state'],
                          envelope_sha256=hashlib.sha256(expected).hexdigest(),
                          marker_files=len(actual), stores_unchanged=True)))


if __name__ == '__main__':
    action, home = sys.argv[1], Path(sys.argv[2])
    if action == 'prepare':
        prepare(home, sys.argv[3])
    else:
        compare(home)
