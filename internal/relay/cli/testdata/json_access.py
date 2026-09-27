"""Live console-entry-point parity for JSON fields, including all SQLite tables.

Only wall-clock fields are normalized. Each run restores the identical database
bytes before the command; neither implementation inherits the other's writes.
"""
import argparse
import copy
from contextlib import closing
import difflib
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import tempfile


# One exercised case per shared operation, plus boundary regressions. Keep this
# list explicit: mutation proofs use the same selection as the default Go test.
REPRESENTATIVE = {
    'dict': 'forge/pull/head/2',
    'text': 'show/origin/True',
    'truth': 'forge/pull/merged/7',
    'int': 'forge/jobs/jobs.0.run_attempt/2',
    'missing-total': 'forge/reviewThreads/data.repository.pullRequest.reviewThreads.totalCount/12',
    'hash': 'forge/checks/check_runs.0.id/9',
    'ascii': 'digest/' + repr('\u00e9'),
    'len': 'render/handoff/checks/True',
    'index': 'forge/reviewThreads/data.repository.pullRequest.reviewThreads.nodes.0.comments.nodes/11',
    'item': 'revision/finding/id/None',
    'items': 'render/evidence/True',
    'marker': 'marker/intent.json/workspace/True',
    'big-int': 'forge/big/jobs',
    'url-quoting': 'forge/big/pull',
    'readback-shape': 'show/detail/True',
    'frozen-reading': 'show/reading/[1]',
    'origin-hash': "show/origin/{'z': 1, 'a': False}",
    'generation': 'observation/executionGeneration/1.5',
    'selector': 'observation/selectors/True',
    'observation-repr': "observation/reason/{'z': 1, 'a': False}",
    'manifest': 'render/manifest/True',
    'restore': 'render/restore/True',
    'restore-skills': 'render/restore/skills/[1]',
    'unresolved': 'render/unresolved/[1]',
    'coverage': 'render/handoff/review_coverage/True',
    'required': 'render/handoff/required_declared/True',
    'dispositions': 'render/handoff/thread_dispositions/True',
    'revision-kind': 'revision/kind/1.5',
    'revision-blockers': 'revision/blockers/True',
    'confirming-pass': 'forge/reviewThreads/data.repository.pullRequest.reviewThreads.nodes.0.isResolved/7',
    'report-valid': 'render/baseline',
    'revision-valid': 'revision/baseline',
}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', required=True)
    parser.add_argument('--binary', required=True)
    parser.add_argument('--filter', default='')
    parser.add_argument('--output')
    parser.add_argument('--representative', action='store_true', help='bounded CI cases; omit for the exhaustive matrix')
    args = parser.parse_args()
    root = Path(args.root)
    oracle = root / '.venv/bin/codex-session-relay'
    binary = Path(args.binary)
    timestamp = re.compile(rb'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00')
    with tempfile.TemporaryDirectory(prefix='json-access-') as tmp:
        home = Path(tmp)
        env = {k: v for k, v in os.environ.items() if not k.startswith(('XDG_', 'CODEX', 'CRW_'))}
        env.update(HOME=tmp, XDG_STATE_HOME=tmp+'/xdg', XDG_CONFIG_HOME=tmp+'/config', XDG_CACHE_HOME=tmp+'/cache', XDG_DATA_HOME=tmp+'/data', CODEX_HOME=tmp+'/codex', CRW_ALLOW_LIVE_STATE='1', PATH=str(root/'internal/relay/cli/testdata')+':'+env['PATH'], CRW_FORGE_SCENARIO='rich')
        state = home/'state'
        state.mkdir()
        # Initialize once through the Python console and retain a byte-identical seed.
        initialized = subprocess.run([str(oracle), '--state', str(state), 'status'], env=env, capture_output=True, timeout=30)
        if initialized.returncode != 0:
            raise RuntimeError(initialized.stdout, initialized.stderr)
        seed = (state/'relay.sqlite3').read_bytes()
        def tables():
            with closing(sqlite3.connect(state/'relay.sqlite3')) as db:
                return {name: db.execute('select * from "'+name+'"').fetchall() for (name,) in db.execute("select name from sqlite_master where type='table' order by name")}
        def restore(sql):
            for path in state.iterdir(): path.unlink()
            (state/'relay.sqlite3').write_bytes(seed)
            with closing(sqlite3.connect(state/'relay.sqlite3')) as db:
                with db:
                    for statement, values in sql: db.execute(statement, values)
        results = []
        def compare(label, argv, extra=None, sql=()):
            if args.filter and not label.startswith(args.filter): return
            if args.representative and label not in REPRESENTATIVE.values(): return
            argv = ['e'*32 if arg == 'e' else arg for arg in argv]
            pair = []
            for command in ([str(oracle)], [str(binary), 'relay']):
                restore(sql)
                p = subprocess.run(command+['--state',str(state)]+argv, env=dict(env, **(extra or {})), capture_output=True, timeout=30)
                pair.append((p.returncode, timestamp.sub(b'<time>',p.stdout), p.stderr, tables()))
            equal = pair[0] == pair[1]
            results.append({'case':label,'equal':equal, 'python_exit':pair[0][0], 'go_exit':pair[1][0], 'tables':len(pair[0][3])})
            if not equal:
                print('DIFF',label, 'exit',pair[0][0],pair[1][0])
                for part, index in [('stdout',1),('stderr',2),('tables',3)]:
                    a,b = pair[0][index],pair[1][index]
                    if a == b: continue
                    if isinstance(a,bytes): a,b=a.decode(errors='backslashreplace'),b.decode(errors='backslashreplace')
                    else: a,b=repr(a),repr(b)
                    print(part, ''.join(difflib.unified_diff(a.splitlines(True),b.splitlines(True),fromfile='Python',tofile='Go')))
        bad = [None, False, True, 0, 2, 1.5, '', 'x', [], [1], {}, {'z':1,'a':False}]
        missing = object()
        forge = ['merge-evidence','--repository','owner/repo','--pull-request','7','--page-budget','2']
        paths = {
            'pull': [[], ['head'], ['head','sha'], ['base'], ['base','ref'], ['merged'], ['draft'], ['state'], ['mergeable_state']],
            'reviewThreads': [[],['data'],['data','repository'],['data','repository','pullRequest'], ['data','repository','pullRequest','reviewThreads']],
            'reviews': [], 'comments': [],
            'runs': [[],['total_count'],['workflow_runs'],['workflow_runs',0],['workflow_runs',0,'id'],['workflow_runs',0,'workflow_id'],['workflow_runs',0,'event'],['workflow_runs',0,'run_started_at'],['workflow_runs',0,'head_sha']],
            'jobs': [[],['total_count'],['jobs'],['jobs',0],['jobs',0,'id'],['jobs',0,'name'],['jobs',0,'run_attempt'],['jobs',0,'status'],['jobs',0,'conclusion']],
            'checks': [[],['total_count'],['check_runs'],['check_runs',0],['check_runs',0,'id'],['check_runs',0,'app'],['check_runs',0,'app','id'],['check_runs',0,'name'],['check_runs',0,'head_sha']],
            'statuses': [[],['total_count'],['statuses'],['statuses',0],['statuses',0,'context'],['statuses',0,'state']],
        }
        for field in ('reviewThreads','reviews','comments'):
            prefix=['data','repository','pullRequest',field]
            paths[field] += [prefix+[key] for key in ('totalCount','nodes','pageInfo')]
            paths[field] += [prefix+['pageInfo',key] for key in ('hasNextPage','endCursor')]
            paths[field] += [prefix+['nodes',0],prefix+['nodes',0,'id'],prefix+['nodes',0,'author'],prefix+['nodes',0,'body']]
            if field == 'reviewThreads': paths[field] += [prefix+['nodes',0,'isResolved'],prefix+['nodes',0,'comments'],prefix+['nodes',0,'comments','nodes'],prefix+['nodes',0,'comments','nodes',0],prefix+['nodes',0,'comments','nodes',0,'author'],prefix+['nodes',0,'comments','nodes',0,'body']]
            else: paths[field] += [prefix+['nodes',0,'author','login']]
        for endpoint, fields in paths.items():
            for path in fields:
                for i,value in enumerate(bad+[missing]):
                    patch: dict[str, object] = {'endpoint':endpoint, 'path':path, 'delete':value is missing}
                    if value is not missing: patch['value']=value
                    compare('forge/'+endpoint+'/'+'.'.join(map(str,path))+'/'+str(i),forge,{'CRW_FORGE_PATCH':json.dumps(patch)})
        for endpoint,path in [('jobs',['jobs',0,'run_attempt']),('pull',['base','ref'])]:
            compare('forge/big/'+endpoint,forge,{'CRW_FORGE_PATCH':json.dumps({'endpoint':endpoint,'path':path,'value':1e300,'delete':False})})
        for value in ('é','a\x7f','😀',{'z':1,'a':False}):
            rules=[{'type':'required_status_checks','parameters':{'required_status_checks':[{'context':value}]}}]
            compare('digest/'+repr(value),forge,{'CRW_FORGE_RULES_JSON':json.dumps(rules)})
        message = ('insert into supervisor_messages(message_id,obligation_id,obligation_kind,relationship_id,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,staged_at,updated_at,reading) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)', ('m','o','unreported','r','completion','notification','p','s','t','{}','read','at','at',None))
        for value in bad:
            detail=json.dumps({'turnOrigin':value})
            sql=[message,('insert into supervisor_readbacks(message_id,read_turn_id,proof,verified,detail,read_at) values(?,?,?,?,?,?)',('m','t','proof','host_read',detail,'at'))]
            compare('show/origin/'+repr(value),['supervisor-show','--message','m'],sql=sql)
            compare('show/detail/'+repr(value),['supervisor-show','--message','m'],sql=[message,(sql[1][0],('m','t','proof','host_read',json.dumps(value),'at'))])
            compare('show/reading/'+repr(value),['supervisor-show','--message','m'],sql=[(message[0],message[1][:-1]+(json.dumps(value),))])
        def insert(table, values):
            if values.get('event_id') == 'e': values['event_id'] = 'e'*32
            if table == 'events': values['receipt'] = values['receipt'].replace('"eventId": "e"', '"eventId": "'+'e'*32+'"')
            keys=list(values)
            return ('insert into '+table+'('+','.join(keys)+') values('+','.join('?' for _ in keys)+')',tuple(values[k] for k in keys))
        receipt={'eventId':'e','relationshipId':'r','executionGeneration':1,'revisionHash':'a'*64,'outcome':'blocked_needs_input','producer':'child','attempt':1,'turnRef':{'threadId':'c','turnId':'t','turnStatus':'completed'},'manifest':[], 'emittedAt':'2026-01-01T00:00:00+00:00'}
        report_sql=[
            insert('relationships',dict(relationship_id='r',issue_key='I',status='active',parent_task_id='p',parent_host_id='h',child_task_id='c',child_host_id='h',execution_generation=1,artifact_roots='[]',allowed_recipients='[]',created_at='at',updated_at='at')),
            insert('events',dict(event_id='e',relationship_id='r',execution_generation=1,revision_hash='a'*64,outcome='blocked_needs_input',producer='child',attempt=1,turn_thread_id='c',turn_id='t',turn_status='completed',receipt=json.dumps(receipt),first_seen_at='at',last_seen_at='at')),
            insert('deliveries',dict(event_id='e',relationship_id='r',kind='completion',recipient_task_id='p',recipient_thread_id='p',state='queued',created_at='at',updated_at='at')),
            insert('work_reports',dict(event_id='e',relationship_id='r',execution_generation=1,revision_hash='a'*64,repository='repo',cxc_status='BLOCKED',cxc_reason='waiting',contract_version='0.2.28+codex.20260914090142',summary='summary',evidence='[]',unresolved='[]',next_action='next',review=None,restore='{}',recorded_at='at')),
            insert('work_report_handoffs',dict(event_id='e',is_draft=0,base_verified_at='at',required_declared='[]',checks='[]',review_coverage='{}',thread_dispositions='[]',criterion_evidence='[]',limitations='[]',recorded_at='at')),
        ]
        # A legacy event with no live relationship exercises the report renderer
        # without the pre-existing preview-context disagreement between engines.
        report_sql = report_sql[1:]
        compare('render/baseline',['show','--event','e','--message'],sql=report_sql)
        for column in ('review_coverage','required_declared','checks','thread_dispositions'):
            for value in bad:
                compare('render/handoff/'+column+'/'+repr(value),['show','--event','e','--message'],sql=report_sql+[(f'update work_report_handoffs set {column}=?',(json.dumps(value),))])
        for column in ('evidence','unresolved','restore'):
            for value in bad:
                compare('render/'+column+'/'+repr(value),['show','--event','e','--message'],sql=report_sql+[(f'update work_reports set {column}=?',(json.dumps(value),))])
        for column, field in [('evidence','check'),('evidence','detail'),('evidence','exitCode'),('unresolved','id'),('unresolved','note'),('restore','mode'),('restore','skills')]:
            for value in bad:
                obj={field:value}
                if column!='restore': obj=[obj]
                compare('render/'+column+'/'+field+'/'+repr(value),['show','--event','e','--message'],sql=report_sql+[(f'update work_reports set {column}=?',(json.dumps(obj),))])
        for value in bad:
            changed=dict(receipt, eventId='e'*32, manifest=value)
            compare('render/manifest/'+repr(value),['show','--event','e','--message'],sql=report_sql+[("update events set receipt=?",(json.dumps(changed),))])
        revision = dict(receipt, eventId='e'*32, outcome='revision_request', producer='parent', criteria=[{'id':'c1','verdict':'needs_changes','note':'fix'}], verdict='needs_changes', supersedesEvent='f'*32, executionGeneration=2)
        revision_sql=report_sql+[("update events set outcome='revision_request',receipt=?",(json.dumps(revision),)),("update deliveries set kind='revision_request'",()),("update work_reports set execution_generation=2,review=?",(json.dumps({'kind':'FAIL','findings':[]}),))]
        compare('revision/baseline',['show','--event','e','--message'],sql=revision_sql)
        for value in bad:
            compare('revision/review/'+repr(value),['show','--event','e','--message'],sql=revision_sql+[("update work_reports set review=?",(json.dumps(value),))])
            for field in ('kind','findings','blockers'):
                review: dict[str, object] = {'kind':'FAIL','findings':[]};review[field]=value
                compare('revision/'+field+'/'+repr(value),['show','--event','e','--message'],sql=revision_sql+[("update work_reports set review=?",(json.dumps(review),))])
            for column in ('evidence','unresolved','restore'):
                compare('revision/'+column+'/'+repr(value),['show','--event','e','--message'],sql=revision_sql+[(f'update work_reports set {column}=?',(json.dumps(value),))])
            for field in ('id','note','anchor','verdict'):
                finding: dict[str, object] = {'id':'c1','verdict':'needs_changes','note':'fix'}; finding[field]=value
                review={'kind':'FAIL','findings':[finding]}
                compare('revision/finding/'+field+'/'+repr(value),['show','--event','e','--message'],sql=revision_sql+[("update work_reports set review=?",(json.dumps(review),))])
        observation_sql=[insert('relationships',dict(relationship_id='r',issue_key='I',status='active',parent_task_id='p',parent_host_id='h',child_task_id='c',child_host_id='h',execution_generation=1,artifact_roots='[]',allowed_recipients='[]',created_at='at',updated_at='at')),insert('relationship_scope',dict(relationship_id='r',project_key='P',recorded_at='at')),insert('generations',dict(relationship_id='r',execution_generation=1,dispatch_request_id='dispatch',anchor_state='bound',dispatch_turn_id='t',opened_at='at'))]
        for value in bad:
            observation=home/'observation.json'
            for field in ('schema','reportingState','relationshipId','selectors','executionGeneration','reason'):
                reading: dict[str, object] = {'schema':'reporting-observation/1','reportingState':'unreported','relationshipId':'r','selectors':{'turn':'t'},'executionGeneration':1}
                reading[field]=value
                observation.write_text(json.dumps(reading))
                compare('observation/'+field+'/'+repr(value),['supervisor-standing','--project','P','--observation',str(observation)],sql=observation_sql)
            observation.write_text(json.dumps(value))
            compare('observation/top/'+repr(value),['supervisor-standing','--project','P','--observation',str(observation)],sql=observation_sql)
        marker=home/'marker'
        workspace=home/'workspace';workspace.mkdir()
        assignment=hashlib.sha256(b'dispatch').hexdigest()
        directory=marker/hashlib.sha256(str(workspace).encode()).hexdigest()/assignment
        directory.mkdir(parents=True)
        facts: dict[str, dict[str, object]] = {'intent.json':{'dispatchRequestIdHash':assignment,'workspace':str(workspace),'dbPath':str(state/'relay.sqlite3'),'issueKey':'I'},'bound.json':{'sessionId':'c','taskId':'c'},'relationship.json':{'relationshipId':'r','executionGeneration':1},'claims/c/claim.json':{'sessionId':'c','dispatchRequestId':'dispatch'}}
        for name, fact in facts.items():
            p=directory/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_text(json.dumps(fact))
        argv=['reporting-show','--marker-root',str(marker),'--workspace',str(workspace),'--assignment',assignment,'--session','c','--turn','t']
        for name, fields in [('intent.json',['workspace','dbPath','issueKey']),('bound.json',['sessionId','taskId']),('relationship.json',['relationshipId','executionGeneration']),('claims/c/claim.json',['sessionId','dispatchRequestId'])]:
            for field in fields:
                for value in bad:
                    fact=copy.deepcopy(facts[name]);fact[field]=value
                    (directory/name).write_text(json.dumps(fact))
                    compare('marker/'+name+'/'+field+'/'+repr(value),argv)
                (directory/name).write_text(json.dumps(facts[name]))
        if args.representative:
            expected = {label for label in REPRESENTATIVE.values() if label.startswith(args.filter)}
            actual = {result['case'] for result in results}
            if actual != expected:
                raise AssertionError(('missing representative cases', expected - actual, actual - expected))
        if not results:
            raise AssertionError('no parity cases selected')
        summary={'cases':len(results),'equal':sum(x['equal'] for x in results),'results':results}
        if args.output: Path(args.output).write_text(json.dumps(summary,indent=2))
        print(json.dumps({k:v for k,v in summary.items() if k!='results'}))
        return 0 if summary['cases']==summary['equal'] else 1

if __name__ == '__main__':
    raise SystemExit(main())
