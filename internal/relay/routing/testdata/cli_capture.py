"""Built-binary CLI QA and live argparse corpus; no frozen rendered help."""
import argparse
import contextlib
import io
import json
import os
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[4]
sys.path[:0] = [str(root/'packages/codex-session-relay/src'),str(root/'packages/codex-thread-bridge/src')]
from codex_session_relay import cli, faults
faults.secrets.token_hex=lambda size=None: bytes(range(32 if size is None else size)).hex()
from codex_session_relay.clock import FakeClock

names = ['product-register','product-bind','product-show','route-policy','route-intake','route-classify','route-reconcile','route-show','route-digest','route-projects','completion-check']
parser=cli.build_parser()
children=next(a.choices for a in parser._actions if isinstance(a,argparse._SubParsersAction))

if sys.argv[1] in ('argv','accepted-argv'):
    state=sys.argv[2]
    cases=[]
    for name in names:
        p=children[name]
        actions=[a for a in p._actions if a.option_strings and a.dest!='help']
        def value(a):
            return '2' if a.type is int else '{}'
        base=[]
        for a in actions:
            if a.required: base.extend([a.option_strings[-1],value(a)])
        variants=[['--help'],['-h'],[],base,base+['--unknown'],base+['--unknown=x'],base+['extra'],base+['--'],base+['--','x']]
        for a in actions:
            flag=a.option_strings[-1]
            if a.required:
                variants.append([v for i,v in enumerate(base) if i not in (base.index(flag),base.index(flag)+1)])
            if isinstance(a,argparse._StoreTrueAction):
                variants.extend([base+[flag],base+[flag+'=true'],base+[flag+'=false']])
            else:
                variants.extend([base+[flag],base+[flag,'-x'],base+[flag,''],base+[flag+'='],base+[flag,value(a),flag,value(a)]])
                if a.type is int: variants.extend([base+[flag,'bad'],base+[flag+'=bad']])
            for n in range(3,len(flag)):
                prefix=flag[:n]
                matching=[b for b in p._actions if any(option.startswith(prefix) for option in b.option_strings)]
                if len(matching)==1:
                    variants.extend([base+[prefix] if isinstance(a,argparse._StoreTrueAction) else base+[prefix,value(a)],base+[prefix+'='+value(a)]])
                    break
            if a.choices:
                variants.extend([base+[flag,'bad'],base+[flag+'=bad']])
        prefixes={option[:n] for a in actions for option in a.option_strings for n in range(3,len(option))}
        for prefix in sorted(prefixes):
            if len([a for a in p._actions if any(option.startswith(prefix) for option in a.option_strings)])>1:
                variants.extend([base+[prefix],base+[prefix+'=x']]);break
        seen=set()
        for args in variants:
            key=tuple(args)
            if key in seen:continue
            seen.add(key)
            # Parsing is the oracle. Successful parses are dispatched separately on real stores.
            for prog in ('codex-session-relay','crw relay'):
                for width in ('80','120',None):
                    if width is None:os.environ.pop('COLUMNS',None)
                    else:os.environ['COLUMNS']=width
                    parser=cli.build_parser();parser.prog=prog
                    sub=next(a for a in parser._actions if isinstance(a,argparse._SubParsersAction))
                    for command,child in sub.choices.items():child.prog=prog+' '+command
                    out,err=io.StringIO(),io.StringIO();code=None
                    with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):
                        try:parser.parse_args([name,*args])
                        except SystemExit as e:code=e.code
                    if code is not None and sys.argv[1]=='argv':cases.append(dict(command=name,args=args,prog=prog,width=width,code=code,stdout=out.getvalue(),stderr=err.getvalue()))
                    if code is None and sys.argv[1]=='accepted-argv':
                        with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):
                            code=cli.main(['--state',state,name,*args])
                        cases.append(dict(command=name,args=args,prog=prog,width=width,code=code,stdout=out.getvalue(),stderr=err.getvalue()))
    print(json.dumps(cases));sys.exit(0)

# The original PRD CLI scenarios' literal snapshots.
alpha={'schema':'product-registry/1','product':'alpha-notes','workspace':'example-ws','team':'ALN','familyLabel':'제품:Alpha Notes','repositories':['example-org/alpha-notes'],'surfaces':{'dev_run':{'method':'CRW managed run tool results','active':True},'verification':{'method':'CRW verification verdicts','active':True},'user_report':{'method':'reports filed through the parent','active':True}},'testTarget':{'team':'TST','project':'proj-test'}}
binding={'schema':'product-binding/1','product':'alpha-notes','kind':'project','ref':'proj-aln-editor','title':'proj-aln-editor title','state':'active','components':['editor','sync'],'source':'linear readback'}
policy={'schema':'routing-policy/1','policy':'project_creation','enabled':True,'minIndependentFixes':2,'requireSharedGoal':True,'requireCompletionCriteria':True,'basis':'CRW-206, Jun 2026-09-22'}
incident={'schema':'product-incident/1','product':'alpha-notes','surface':'dev_run','phase':'development','component':'editor','symptom':'cursor_jump','severity':'broken','occurrenceKey':'run-1:step-4'}
reading={'schema':'completion-reading/1','product':'alpha-notes','subject':'ALN-9','claims':{'linearDone':True,'prMerged':True},'requires':{'acceptance':True,'install':False,'realUse':False},'observed':{'acceptance':'absent'}}
state=sys.argv[2]
cli.SystemClock=FakeClock
out=[]
def run(*argv):
    stdout,stderr=io.StringIO(),io.StringIO()
    with contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr):code=cli.main(['--state',state,*argv])
    import sqlite3
    with sqlite3.connect(str(pathlib.Path(state)/'relay.sqlite3')) as db:
        db.row_factory=sqlite3.Row
        tables={r[0]:[dict(row) for row in db.execute('SELECT * FROM "'+r[0]+'" ORDER BY rowid')] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='schema_meta' ORDER BY name").fetchall()}
    out.append(dict(args=list(argv),code=code,stdout=stdout.getvalue(),stderr=stderr.getvalue(),tables=json.dumps(tables,sort_keys=True,ensure_ascii=False)))
    return json.loads(stdout.getvalue())
mode=sys.argv[1]
if mode=='PRD-13':
    from unittest import mock
    from codex_session_relay import ledger_port
    run('product-register','--record',json.dumps(alpha))
    run('product-bind','--record',json.dumps(binding))
    with mock.patch.object(ledger_port,'missing',return_value=['FaultLedger.adopt','FaultLedger.record(adopt=)','faults.UNASSIGNED']):
        run('route-intake','--incident',json.dumps(incident))
        run('route-show','--attention')
        run('route-digest')
        run('route-projects','--product','alpha-notes')
    print(json.dumps(out,ensure_ascii=False));sys.exit(0)
if mode=='project-kind':
    gamma=dict(alpha,product='gamma-kit',team='GMK',familyLabel='product:gamma-kit',repositories=['example-org/gamma-kit'],surfaces={'real_use':{'method':'error events the product forwards','active':True}},testTarget=None)
    run('product-register','--record',json.dumps(gamma))
    run('route-policy','--record',json.dumps(policy))
    for component,symptom,key in [('cache','stale','g1'),('queue','lost','g2')]:
        run('route-intake','--incident',json.dumps(dict(incident,product='gamma-kit',surface='real_use',phase='in_use',repository='example-org/gamma-kit',component=component,symptom=symptom,occurrenceKey=key,goal={'key':'offline_sync','criteria':'edits survive reconnect'})))
    module=('--kind-module','codex_session_relay.projects')
    offered=run(*module,'fault-next','--limit','50')
    pub=next(p for p in offered['publications'] if p['kind']=='project_create')['publication_id']
    fault=next(p for p in offered['publications'] if p['kind']=='project_create')['fault_id']
    run(*module,'fault-queue','--fault',fault,'--kind','project_create','--trigger','invalid','--payload','{}')
    run(*module,'fault-operation','--publication','missing','--claim-token','missing')
    token=run(*module,'fault-claim','--publication',pub,'--owner','holder')['claimToken']
    operation=run(*module,'fault-operation','--publication',pub,'--claim-token',token)
    run(*module,'fault-complete','--publication',pub,'--claim-token',token,'--readback',operation['block'],'--external-ref','proj-new-1','--observed-fields',json.dumps({'team':'GMK'}))
    run('route-digest')
    print(json.dumps(out,ensure_ascii=False));sys.exit(0)
if mode in ('PRD-2','PRD-16'):
    if mode=='PRD-2':
        run('route-policy','--record',json.dumps(dict(policy,basis='')))
        run('route-policy','--record',json.dumps(policy))
    else:
        run('product-register','--record','{not json')
        run('product-register','--record','@'+state+'/absent.json')
    print(json.dumps(out,ensure_ascii=False));sys.exit(0)
if mode=='PRD-10':
    run('product-bind','--record',json.dumps(binding))
    run('product-register','--record',json.dumps(alpha))
    pathlib.Path(state,'binding.json').write_text(json.dumps(binding))
    run('product-bind','--record','@'+state+'/binding.json')
    run('product-show','--product','alpha-notes')
    print(json.dumps(out,ensure_ascii=False));sys.exit(0)
run('product-bind','--record',json.dumps(binding))
run('product-register','--record','{not json')
run('product-register','--record','@'+state+'/absent.json')
run('product-register','--record',json.dumps(alpha))
run('product-bind','--record',json.dumps(binding))
run('product-show','--product','alpha-notes')
run('product-show','--product','omega')
run('route-policy','--record',json.dumps(dict(policy,basis='')))
run('route-policy','--record',json.dumps(policy))
run('route-projects','--product','alpha-notes')
run('route-projects','--product','omega')
run('route-intake','--incident',json.dumps(incident))
run('route-intake','--incident',json.dumps(incident))
pending=run('route-intake','--incident',json.dumps(dict(incident,product=None,repository='example-org/nobody',occurrenceKey='unknown-1')))
run('route-classify','--fault','missing','--classification',json.dumps({'product':'alpha-notes','by':'operator'}))
run('route-classify','--fault',pending['faultId'],'--classification',json.dumps({'product':'alpha-notes','by':'operator'}))
run('route-classify','--fault',pending['faultId'],'--classification',json.dumps({'product':'alpha-notes','by':'operator'}))
run('route-reconcile')
run('route-show','--attention')
run('route-digest')
run('route-digest')
run('completion-check','--reading',json.dumps(reading))
run('product-bind','--record',json.dumps(dict(binding,kind='issue',ref='ALN-9',title='ALN-9 title',state='done',project='proj-aln-editor')))
run('completion-check','--reading',json.dumps(reading))
print(json.dumps(out,ensure_ascii=False))
