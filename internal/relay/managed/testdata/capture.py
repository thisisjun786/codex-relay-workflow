"""Live Python managed-start fake capture. Run under uv with isolated HOME/XDG/CODEX_HOME.

Arguments: tree, scenario. Writes Python's entire public receipt and managed tables;
paths and physical inode fingerprints are normalised only where they differ across
independent Python/Go stores.
"""
import json
import os
import sqlite3
import sys
from pathlib import Path

from codex_session_relay import rolepolicy
from codex_session_relay.managed import ManagedStart
from codex_session_relay.store import Store
from codex_session_relay.clock import FakeClock
from tests.support import task_settings, PARENT, HOST
from tests.test_rolepolicy import write_policy, CHILD_EFFORT, CHILD_MODEL, PARENT_MODEL, PARENT_EFFORT
from tests.test_managed_start import Host

root, scenario = sys.argv[1:3]
base = Path(root)
base.mkdir(parents=True, exist_ok=True)
workspace = base / 'workspace'
workspace.mkdir(exist_ok=True)
state = base / 'state'
if scenario == 'execution-cli':
    from codex_session_relay.cli import main
    import contextlib
    import io
    selection = base / 'execution'
    marker = base / 'execution-markers'
    work = base / 'execution-workspace'
    work.mkdir()
    steps = []
    def command(*args):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            exit_code = main(['--state', str(selection), *args])
        result = {'exit': exit_code, 'stdout': json.loads(output.getvalue())}
        steps.append(result)
        return result['stdout']
    before = command('doctor', '--issue', 'REL-EXECUTION')
    steps.append({'storeExists': (selection / 'relay.sqlite3').exists()})
    declared = command('intent-declare', '--dispatch-request-id', 'dispatch-1', '--issue', 'REL-EXECUTION', '--marker-root', str(marker), '--workspace', str(work))
    assignment = declared['assignmentId']
    relation = command('register', '--parent-task', 'parent', '--parent-host', 'host', '--child-task', 'child', '--child-host', 'host', '--issue', 'REL-EXECUTION', '--artifact-root', str(work), '--allowed-recipient', 'parent', '--dispatch-request-id', 'dispatch-1', '--dispatch-turn-id', 'standby')
    rid = relation['relationshipId']
    command('intent-register', '--assignment', assignment, '--relationship', rid, '--dispatch-request-id', 'dispatch-1', '--db-path', str(selection / 'relay.sqlite3'), '--marker-root', str(marker), '--workspace', str(work))
    command('criteria-register', '--relationship', rid, '--criterion', 'c1=the deliverable behaves')
    command('doctor', '--issue', 'REL-EXECUTION')
    command('assignment-find', '--issue', 'REL-EXECUTION')
    command('intent-show', '--assignment', assignment, '--marker-root', str(marker), '--workspace', str(work))
    command('register', '--parent-task', 'parent', '--parent-host', 'host', '--child-task', 'other-child', '--child-host', 'host', '--issue', 'REL-EXECUTION', '--artifact-root', str(work), '--allowed-recipient', 'parent', '--dispatch-request-id', 'dispatch-2', '--dispatch-turn-id', 'standby')
    command('assignment-find', '--issue', 'REL-EXECUTION')
    (base / 'capture.json').write_text(json.dumps({'receipts': steps, 'effects': [0, 0], 'tables': {}, 'ledger': {'path':'', 'device':0, 'inode':0}}))
    sys.exit(0)
store = Store(state / 'relay.sqlite3')
clock = FakeClock()
policy = write_policy(base)
os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = policy
child = task_settings(str(workspace), model=CHILD_MODEL, reasoningEffort=CHILD_EFFORT, environments=[])
request = {
    'schema': 'managed-start/1', 'requestId': 'managed-1', 'issueKey': 'REL-MANAGED',
    'parent': {'taskId': PARENT, 'hostId': HOST, 'settings': task_settings(str(workspace), model=PARENT_MODEL, reasoningEffort=PARENT_EFFORT, environments=[])},
    'child': {'hostId': HOST, 'title': 'REL-MANAGED Verify admission', 'settings': child},
    'artifactRoots': [str(workspace)], 'allowedRecipients': [PARENT],
    'criteria': [{'id': 'c1', 'title': 'preserve replay identity', 'required': True}],
    'criteriaSource': 'issue:REL-MANAGED', 'baselineRevision': 'baseline',
    'scopeRef': 'issue:REL-MANAGED', 'prompt': 'business-secret: implement the scoped fix',
}
host = Host(child)
# The Go fake uses the same path/identity on its independent state store.
ledger = base / 'workspace' / 'test-operations-ledger'
ledger_stat = ledger.stat()
observation = {'observed': True, 'policy': rolepolicy.declared().summary()}
start = ManagedStart(store, clock, host, lambda: observation,
                     socket=str(base / 'socket'), marker_root=str(base / 'markers'),
                     state_selector=str(state))
if scenario in ('reservation-replay', 'reservation-contention', 'reservation-receipts', 'reservation-release', 'reservation-settings', 'reservation-attach', 'reservation-active-owner', 'reservation-raw-register', 'reservation-attach-conflict', 'reservation-checkpoint', 'reservation-index', 'reservation-resume', 'reservation-threads', 'reservation-arm-release'):
    from codex_session_relay.registry import Registry
    identity = {'request_id': 'req-1', 'issue_key': 'REL-1',
                'request_fingerprint': 'fp-1', 'fingerprint_version': 'managed-start/1',
                'workspace': '/tmp/work', 'marker_root': '/tmp/markers',
                'socket_identity': '/tmp/socket', 'create_request_id': 'create-1',
                'dispatch_request_id': 'business-1'}
    registry = Registry(store, clock)
    if scenario == 'reservation-arm-release':
        import threading
        from codex_session_relay.errors import RelayError
        first = registry.reserve_start(identity)
        barrier = threading.Barrier(2)
        outcomes = []
        def competing(action):
            connection = Store(state / 'relay.sqlite3')
            try:
                barrier.wait()
                try:
                    other = Registry(connection, FakeClock())
                    row = (other.arm_start('req-1', 'fp-1', 0) if action == 'arm' else
                           other.release_unstarted('req-1', 'fp-1', 0, 'operator'))
                    result = {'ok': row}
                except RelayError as error:
                    result = {'error': type(error).__name__, 'detail': str(error)}
                outcomes.append(result)
            finally:
                connection.close()
        threads = [threading.Thread(target=competing, args=(name,)) for name in ('arm','release')]
        for thread in threads: thread.start()
        for thread in threads: thread.join()
        receipts = [first, *sorted(outcomes, key=lambda item: 'ok' not in item), registry.start_request('req-1')]
    elif scenario == 'reservation-threads':
        import threading
        from codex_session_relay.errors import RelayError
        barrier = threading.Barrier(2)
        outcomes = []
        def competing(name):
            connection = Store(state / 'relay.sqlite3')
            try:
                barrier.wait()
                try:
                    row = Registry(connection, FakeClock()).reserve_start({**identity, 'request_id': name})
                    result = {'ok': row}
                except RelayError as error:
                    result = {'error': type(error).__name__, 'detail': str(error)}
                outcomes.append(result)
            finally:
                connection.close()
        threads = [threading.Thread(target=competing, args=(name,)) for name in ('req-a','req-b')]
        for thread in threads: thread.start()
        for thread in threads: thread.join()
        receipts = sorted(outcomes, key=lambda item: 'ok' not in item)
    elif scenario == 'reservation-resume':
        from codex_session_relay.models import Endpoint
        first = registry.reserve_start(identity)
        store.db.execute("UPDATE managed_start_requests SET state='released',revision=1,release_reason='make-room' WHERE request_id='req-1'")
        relation = registry.register(parent=Endpoint('parent', 'host', cwd='/parent'),
            child=Endpoint('child', 'host', cwd='/tmp/work'), issue_key='REL-1',
            artifact_roots=['/tmp/work'], allowed_recipients=['parent'], dispatch_request_id='live')
        registry.set_status(relation['relationshipId'], 'paused', actor='operator')
        store.db.execute("UPDATE managed_start_requests SET state='reserved',revision=0,release_reason=NULL WHERE request_id='req-1'")
        receipts = [first, {'relationshipId': relation['relationshipId']}]
        try:
            registry.resume(relation['relationshipId'], expect_generation=1,
                expect_artifact_roots=['/tmp/work'], expect_allowed_recipients=['parent'], actor='operator')
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
    elif scenario == 'reservation-index':
        receipts = [registry.reserve_start(identity)]
        try:
            store.db.execute("INSERT INTO managed_start_requests (request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,'reserved',0,?,?)",
                ('req-direct', 'REL-1', 'fp-1', 'managed-start/1', '/tmp/work', '/tmp/markers', '/tmp/socket', 'create-x', 'business-x', clock.iso(), clock.iso()))
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
    elif scenario == 'reservation-checkpoint':
        receipts = [registry.reserve_start(identity), registry.arm_start('req-1', 'fp-1', 0),
                    registry.arm_start('req-1', 'fp-1', 0),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'accepted', 'threadId': 'child', 'turnId': 'standby'}),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'accepted', 'threadId': 'child', 'turnId': 'standby'}),
                    registry.reserve_start(identity)]
    elif scenario == 'reservation-attach-conflict':
        from codex_session_relay.models import Endpoint
        receipts = [registry.reserve_start(identity), registry.arm_start('req-1', 'fp-1', 0),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'accepted', 'threadId': 'child', 'turnId': 'standby'})]
        try:
            registry.register(parent=Endpoint('parent', 'host', cwd='/parent'),
                child=Endpoint('other', 'host', cwd='/tmp/work'), issue_key='REL-1',
                artifact_roots=['/tmp/work'], allowed_recipients=['parent'],
                dispatch_request_id='business-1', dispatch_turn_id='standby', managed_request_id='req-1')
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
    elif scenario == 'reservation-raw-register':
        from codex_session_relay.models import Endpoint
        receipts = [registry.reserve_start(identity)]
        try:
            registry.register(parent=Endpoint('parent', 'host', cwd='/parent'),
                child=Endpoint('other', 'host', cwd='/tmp/work'), issue_key='REL-1',
                artifact_roots=['/tmp/work'], allowed_recipients=['parent'],
                dispatch_request_id='raw')
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
    elif scenario == 'reservation-active-owner':
        from codex_session_relay.models import Endpoint
        relation = registry.register(parent=Endpoint('parent', 'host', cwd='/parent'),
            child=Endpoint('child', 'host', cwd='/tmp/work'), issue_key='REL-1',
            artifact_roots=['/tmp/work'], allowed_recipients=['parent'],
            dispatch_request_id='live')
        receipts = [{'relationshipId': relation['relationshipId']}]
        for status in ('active', 'paused'):
            if status == 'paused': registry.set_status(relation['relationshipId'], 'paused', actor='operator')
            try:
                registry.reserve_start(identity)
            except Exception as error:
                receipts.append({'error': type(error).__name__, 'detail': str(error)})
    elif scenario == 'reservation-attach':
        from codex_session_relay.models import Endpoint
        receipts = [registry.reserve_start(identity), registry.arm_start('req-1', 'fp-1', 0),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'accepted', 'threadId': 'child', 'turnId': 'standby'})]
        relation = registry.register(parent=Endpoint('parent', 'host', cwd='/parent'),
            child=Endpoint('child', 'host', cwd='/tmp/work'), issue_key='REL-1',
            artifact_roots=['/tmp/work'], allowed_recipients=['parent'],
            dispatch_request_id='business-1', dispatch_turn_id='standby', managed_request_id='req-1')
        receipts.append({'relationshipId': relation['relationshipId']})
        receipts.append(registry.start_request('req-1'))
    elif scenario == 'reservation-settings':
        from codex_session_relay.registry import record_settings
        from codex_session_relay.errors import RelayError
        settings = request['parent']['settings']
        receipts = [record_settings(store, clock, 'parent', settings, source='creation_result'),
                    record_settings(store, clock, 'parent', settings, source='recovery', ensure_only=True)]
        changed = {**settings, 'model': 'other-model'}
        try:
            record_settings(store, clock, 'parent', changed, source='recovery', ensure_only=True)
        except RelayError as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(record_settings(store, clock, 'task-new', settings, source='recovery', ensure_only=True))
    elif scenario == 'reservation-release':
        receipts = [registry.reserve_start(identity), registry.release_unstarted('req-1', 'fp-1', 0, 'stopped')]
        try:
            registry.reserve_start(identity)
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
        registry.reserve_start({**identity, 'request_id': 'req-armed', 'issue_key': 'REL-ARMED'})
        registry.arm_start('req-armed', 'fp-1', 0)
        try:
            registry.release_unstarted('req-armed', 'fp-1', 1, 'stopped')
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-armed'))
    elif scenario == 'reservation-receipts':
        receipts = [registry.reserve_start(identity), registry.arm_start('req-1', 'fp-1', 0),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'unknown', 'threadId': 'child'}),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'accepted', 'threadId': 'child'}),
                    registry.record_start_receipt('req-1', 'fp-1', {'status': 'accepted', 'threadId': 'child', 'turnId': 'standby'})]
    elif scenario == 'reservation-replay':
        receipts = [registry.reserve_start(identity), registry.reserve_start(identity)]
        try:
            registry.reserve_start({**identity, 'request_fingerprint': 'different'})
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
    else:
        receipts = [registry.reserve_start(identity)]
        try:
            registry.reserve_start({**identity, 'request_id': 'req-2'})
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
        receipts.append(registry.start_request('req-1'))
    store.close()
    db = sqlite3.connect(state / 'relay.sqlite3')
    db.row_factory = sqlite3.Row
    tables = {table: [dict(row) for row in db.execute(f'SELECT * FROM {table} ORDER BY rowid')]
              for table in ('managed_start_requests', 'relationships', 'generations',
                            'canonical_criteria', 'verification_mode', 'authorized_settings',
                            'generation_turns')}
    db.close()
    (base / 'capture.json').write_text(json.dumps({'receipts': receipts, 'effects': [0, 0],
        'tables': tables, 'ledger': {'path': str(ledger), 'device': ledger_stat.st_dev,
                                     'inode': ledger_stat.st_ino}}))
    sys.exit(0)
if scenario == 'unknown':
    host.creation_status = 'outcome_unknown'
elif scenario == 'standby' or scenario == 'crash-criteria' or scenario.startswith('guard-'):
    host.standby = 'inProgress'
elif scenario == 'ledger-retry':
    host.creation_status = 'outcome_unknown'
elif scenario == 'missing-worker':
    observation = {'observed': False, 'reason': 'worker_policy_unconfigured'}
elif scenario == 'unknown-ledger':
    host.ledger_identity_record = lambda: None
elif scenario == 'unsupported-policy':
    request['child']['settings']['sandbox'] = {'type': 'readOnly', 'networkAccess': True}
elif scenario == 'ledger-replaced':
    original_arm = start.registry.arm_start
    def replace_after_arm(*args):
        row = original_arm(*args)
        replacement = host.ledger_path.with_suffix('.replacement')
        replacement.touch()
        replacement.replace(host.ledger_path)
        return row
    start.registry.arm_start = replace_after_arm
elif scenario == 'paused':
    host.paused = True
elif scenario == 'environment':
    host.settings = {**child, 'environments': [{'environmentId': 'local', 'cwd': str(workspace), 'runtimeWorkspaceRoots': [str(workspace)]}]}
elif scenario == 'approval-drift':
    host.settings = {**child, 'approvalPolicy': 'on-request'}
elif scenario == 'approval-drift-partial':
    host.settings = {**child, 'approvalPolicy': 'on-request'}
    host.creation_status = 'failed'
    original_create = host.create_thread
    def partial(request_id, **kwargs):
        receipt = original_create(request_id, **kwargs)
        receipt['attemptedEffects'] = ['thread/start', 'thread/name/set']
        receipt.pop('turnId')
        return receipt
    host.create_thread = partial
elif scenario == 'uncertain-turn':
    host.creation_status = 'failed'
    original_create = host.create_thread
    def partial(request_id, **kwargs):
        receipt = original_create(request_id, **kwargs)
        receipt['attemptedEffects'] = ['thread/start', 'thread/name/set', 'turn/start']
        receipt.pop('turnId')
        return receipt
    host.create_thread = partial
elif scenario == 'unknown-recovery':
    host.creation_status = 'failed'
    original_create = host.create_thread
    def partial(request_id, **kwargs):
        receipt = original_create(request_id, **kwargs)
        receipt['attemptedEffects'] = ['thread/start', 'thread/name/set']
        receipt.pop('turnId')
        return receipt
    host.create_thread = partial
    def unknown_send(request_id, task, message, settings, **kwargs):
        host.sent += 1
        receipt = {'status': 'outcome_unknown', 'threadId': task}
        host.operations[request_id] = receipt
        return receipt
    host.send_message = unknown_send
elif scenario == 'paused-partial':
    host.creation_status = 'failed'
    host.paused = True
    original_create = host.create_thread
    def partial(request_id, **kwargs):
        receipt = original_create(request_id, **kwargs)
        receipt['attemptedEffects'] = ['thread/start', 'thread/name/set']
        receipt.pop('turnId')
        return receipt
    host.create_thread = partial
elif scenario == 'policy-disappears':
    original_create = host.create_thread
    def create_and_disappear(request_id, **kwargs):
        global observation
        receipt = original_create(request_id, **kwargs)
        observation = {'observed': False, 'reason': 'worker_policy_unconfigured'}
        return receipt
    host.create_thread = create_and_disappear
elif scenario in ('relationship-paused','settings-drift','prompt-changed','input-changes','selector-spellings'):
    host.standby = 'inProgress'
elif scenario == 'creation-policy-drift':
    host.settings = {**child, 'approvalPolicy': 'on-request'}
elif scenario == 'recipient-predeclared':
    request['allowedRecipients'].append('child-new')
elif scenario == 'naming':
    host.creation_status = 'failed'
    original_create = host.create_thread
    def partial(request_id, **kwargs):
        receipt = original_create(request_id, **kwargs)
        receipt['attemptedEffects'] = ['thread/start', 'thread/name/set']
        receipt.pop('turnId')
        return receipt
    host.create_thread = partial
    original_send = host.send_message
    def recovered(request_id, task, message, settings, **kwargs):
        receipt = original_send(request_id, task, message, settings, **kwargs)
        if request_id.startswith('managed-standby-'):
            receipt['turnId'] = 'recovered-standby'
        return receipt
    host.send_message = recovered
try:
    receipts = [start.run(request)]
except Exception as error:
    receipts = [{'error':type(error).__name__, 'detail':str(error)}]
if scenario in ('happy', 'unknown', 'naming', 'uncertain-turn', 'unknown-recovery', 'paused-partial'):
    receipts.append(start.run(request))
if scenario == 'standby':
    host.standby = 'completed'
    receipts.append(start.run(request))
if scenario == 'ledger-retry':
    replacement = host.ledger_path.with_suffix('.replacement')
    replacement.touch()
    replacement.replace(host.ledger_path)
    host.operations.clear()
    try:
        receipts.append(start.run(request))
    except Exception as error:
        receipts.append({'error': type(error).__name__, 'detail': str(error)})
if scenario == 'policy-disappears':
    observation = {'observed': True, 'policy': rolepolicy.declared().summary()}
    receipts.append(start.run(request))
if scenario == 'crash-criteria':
    host.standby = 'completed'
    receipts.append(start.run(request))
if scenario == 'crash-business':
    receipts.append(start.run(request))
if scenario == 'recipient-scope':
    from codex_session_relay.scope import assert_assignment_delivery
    from codex_session_relay.registry import Registry
    relation = Registry(store, clock).get(receipts[0]['relationshipId'])
    findings = []
    for kind, recipient in (('revision_request','child-new'),('completion_event',PARENT),('revision_request','unrelated')):
        try:
            assert_assignment_delivery(relation, kind=kind, recipient_task_id=recipient)
            findings.append({'kind':kind,'recipient':recipient,'result':'allowed'})
        except Exception as error:
            findings.append({'kind':kind,'recipient':recipient,'error':type(error).__name__,'detail':str(error)})
if scenario == 'relationship-paused':
    from codex_session_relay.registry import Registry
    Registry(store, clock).set_status(receipts[0]['relationshipId'], 'paused', actor=PARENT)
    receipts.append(start.run(request))
if scenario in ('prompt-changed','input-changes','selector-spellings'):
    import copy
    variations = []
    if scenario == 'prompt-changed':
        changed = copy.deepcopy(request); changed['prompt'] = 'different assignment'; variations.append((changed, {}))
    elif scenario == 'input-changes':
        for field, replacement in (('criteriaSource','new-source'),('scopeRef','new-scope'),('baselineRevision','new-base'),('prompt','different')):
            changed = copy.deepcopy(request); changed[field] = replacement; variations.append((changed, {}))
        changed = copy.deepcopy(request); changed['criteria'][0]['required'] = False; variations.append((changed, {}))
    else:
        alias = base / 'spelling'; alias.mkdir()
        for kw in ({'socket':str(alias)+'/../socket'}, {'marker_root':str(alias)+'/../markers'}, {'state_selector':str(alias)+'/..'}):
            variations.append((request, kw))
    for changed, selectors in variations:
        runner = start if not selectors else ManagedStart(store, clock, host, lambda: observation,
            socket=selectors.get('socket',str(base/'socket')),
            marker_root=selectors.get('marker_root',str(base/'markers')),
            state_selector=selectors.get('state_selector',str(state)))
        try:
            receipts.append(runner.run(changed))
        except Exception as error:
            receipts.append({'error': type(error).__name__, 'detail': str(error)})
if scenario == 'cli-missing-selectors':
    from codex_session_relay.cli import main
    import contextlib
    import io
    for selectors in ([], ['--state', str(base / 'absent')], ['--socket', str(base / 'socket')]):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = main([*selectors, 'managed-start', '--request', '{}', '--marker-root', str(base / 'markers')])
        receipts.append({'exit': code, 'stdout': json.loads(output.getvalue())})
if scenario == 'cli-missing-worker':
    from codex_session_relay.cli import main
    import contextlib
    import io
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        code = main(['--state', str(base / 'absent'), '--socket', str(base / 'socket'),
                     'managed-start', '--request', json.dumps(request), '--marker-root', str(base / 'markers')])
    receipts.append({'exit': code, 'stdout': json.loads(output.getvalue()),
                     'storeExists': (base / 'absent' / 'relay.sqlite3').exists()})
if scenario == 'cli-unknown-input':
    from codex_session_relay.cli import main
    import contextlib
    import io
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        code = main(['--state', str(base / 'absent'), '--socket', str(base / 'socket'),
                     'managed-start', '--request', json.dumps({**request, 'overridePermissions': True}),
                     '--marker-root', str(base / 'markers')])
    receipts.append({'exit': code, 'stdout': json.loads(output.getvalue()),
                     'storeExists': (base / 'absent' / 'relay.sqlite3').exists()})
if scenario.startswith('host-ready-'):
    from codex_session_relay.managed import HOST_READY_RPC_REQUESTS
    import asyncio
    task = receipts[0]['childTaskId']
    class HostReadyRpc:
        def __init__(self): self.calls = []
        async def call(self, method, params):
            self.calls.append(method)
            if method == 'thread/list':
                present = scenario == 'host-ready-recipient_archived' if params['archived'] else scenario != 'host-ready-recipient_archived' and scenario != 'host-ready-lifecycle_unknown'
                return {'data': [{'id': task}] if present else []}
            if method == 'thread/goal/get': return {'goal': {'status': 'paused'} if scenario == 'host-ready-recipient_paused' else None}
            if method == 'thread/read': return {'thread': {'status': {'type': 'busy' if scenario == 'host-ready-recipient_not_idle' else 'idle'}, 'canAcceptDirectInput': scenario != 'host-ready-recipient_cannot_accept_input'}}
            raise AssertionError(method)
    rpc = HostReadyRpc()
    receipts.append({'guard': asyncio.run(start._host_ready(rpc,task)), 'calls':rpc.calls,'rpcBudget':HOST_READY_RPC_REQUESTS})
if scenario.startswith('guard-'):
    from codex_session_relay.managed import HOST_READY_RPC_REQUESTS
    import asyncio
    task = receipts[0]['childTaskId']
    if scenario == 'guard-pause':
        import threading
        worker_result = {}
        class PausedRpc:
            async def call(self, method, params):
                if method == 'thread/list': return {'data': [] if params['archived'] else [{'id': task}], 'nextCursor': None}
                if method == 'thread/goal/get': return {'goal': {'status': 'paused'}}
                raise AssertionError(method)
        def worker():
            try: worker_result['guard'] = asyncio.run(start._before_start(PausedRpc()))
            except BaseException as error: worker_result['error'] = str(error)
        thread = threading.Thread(target=worker)
        thread.start(); thread.join()
        receipts.append({'guard': worker_result.get('guard'), 'workerError': worker_result.get('error'),
                         'workerFinished': not thread.is_alive(), 'rpcBudget': HOST_READY_RPC_REQUESTS})
    class Rpc:
        def __init__(self): self.calls = []
        async def call(self, method, params):
            self.calls.append(method)
            if method == 'thread/list':
                if scenario == 'guard-budget':
                    page = self.calls.count('thread/list')
                    # Four archived pages followed by four unarchived pages.
                    return {'data': [{'id': task}] if page == 8 else [], 'nextCursor': str(page) if page % 4 else None}
                return {'data': [] if params['archived'] else [{'id': task}], 'nextCursor': None}
            if method == 'thread/goal/get': return {'goal': {'status': 'paused'} if scenario == 'guard-pause' else None}
            if method == 'thread/read': return {'thread': {'id': task, 'status': {'type': 'idle'}, 'canAcceptDirectInput': True}}
            raise AssertionError(method)
    if scenario.startswith('guard-drift-'):
        column, value = {'parent': ('parent_task_id','replacement-parent'), 'host': ('child_host_id','other-host'),
            'roots': ('artifact_roots',json.dumps(['/other-scope'])),
            'recipients': ('allowed_recipients',json.dumps(['unrelated']))}[scenario.removeprefix('guard-drift-')]
        store.db.execute(f'UPDATE relationships SET {column}=? WHERE relationship_id=?', (value, receipts[0]['relationshipId']))
    if scenario != 'guard-pause':
        rpc = Rpc()
        receipts.append({'guard': asyncio.run(start._before_start(rpc)), 'calls': rpc.calls,
                         'rpcBudget': HOST_READY_RPC_REQUESTS})
if scenario == 'show-absent':
    from codex_session_relay.cli import main
    import contextlib
    import io
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        code = main(['--state', str(base / 'absent'), 'managed-show', '--request-id', 'missing'])
    receipts.append({'exit': code, 'stdout': json.loads(output.getvalue()),
                     'storeExists': (base / 'absent' / 'relay.sqlite3').exists()})
if scenario == 'show-after-start':
    from codex_session_relay.cli import main
    import contextlib
    import io
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        code = main(['--state', str(state), 'managed-show', '--request-id', request['requestId']])
    receipts.append({'exit': code, 'stdout': json.loads(output.getvalue())})
if scenario == 'settings-drift':
    from codex_session_relay.registry import record_settings
    changed = {**child, 'sandbox': {**child['sandbox'], 'networkAccess': True}}
    record_settings(store, clock, receipts[0]['childTaskId'], changed,
                    source='user_transition', role='child')
    try:
        receipts.append(start.run(request))
    except Exception as error:
        receipts.append({'error': type(error).__name__, 'detail': str(error)})
store.close()
db = sqlite3.connect(state / 'relay.sqlite3')
db.row_factory = sqlite3.Row
tables = {}
for table in ('managed_start_requests', 'relationships', 'generations', 'canonical_criteria', 'verification_mode', 'authorized_settings', 'generation_turns'):
    tables[table] = [dict(row) for row in db.execute(f'SELECT * FROM {table} ORDER BY rowid')]
db.close()
(base / 'capture.json').write_text(json.dumps({'receipts': receipts, 'effects': [host.created, host.sent], 'tables': tables, 'ledger': {'path': str(ledger), 'device': ledger_stat.st_dev, 'inode': ledger_stat.st_ino}, 'scope': findings if scenario == 'recipient-scope' else None, 'orderedReceipts': [json.dumps(receipt, ensure_ascii=False, separators=(',', ':')) for receipt in receipts]}))
