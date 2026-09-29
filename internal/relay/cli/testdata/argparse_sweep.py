# The live oracle generates cases from its own parser, never the Go spec.
import argparse, contextlib, io, json, os, sys, pathlib, shutil
from codex_session_relay import cli
request=json.load(sys.stdin)
root=cli.build_parser()
cli.build_parser=lambda: root
main=cli.main
children=next(a.choices for a in root._actions if isinstance(a,argparse._SubParsersAction))
results=[]
if request.get('root'):
 cases=[('none',[]),('help',['--help']),('unknown',['--unknown']),('bad',['zzz']),
        ('abbreviation',['--st','{home}/root-state','status']),
        ('ambiguous',['--s','x','status']),('end',['--']),('end-command',['--','status']),
        ('missing-state',['--state']),('json-value',['--json=1','status']),
        ('missing-module',['--kind-module']),('subcommand-abbreviation',['stat']),
        ('space-state-equals',['--state={home}/x y','--kind-module','crw_parity_absent','status']),
        ('space-state-abbreviation',['--sta={home}/x y','--kind-module','crw_parity_absent','status'])]
 for prefix in ([],['--unknown']):
  for tail in (['status'],['status','--help'],['status','--unknown'],['show'],['show','--help'],['region-show','--help']):
   cases.append(('unknown-precedence-'+str(prefix+tail),prefix+tail))
 for command in ('status','supervisor-show','linkage-show','ack-proof','fault-target','capacity-show','region-show'):
  for option in ('--st','--soc','--kind-m','--j','--s','--json=1','--kind-module=crw_parity_absent'):
   tail=[command,'--help']
   if command=='capacity-show': tail=[command]
   if command=='region-show': tail=[command,'--repository','x']
   value=[] if option in ('--j','--json=1','--kind-module=crw_parity_absent') else ['x']
   cases.append((command+'-'+option,['--state','{home}/root-family','--kind-module','crw_parity_absent',option]+value+tail))
 # Each root case runs in a home of its own, empty in both runtimes: a store one case
 # created would otherwise be absent for this oracle's earlier cases and present for all of Go's.
 environ,cwd=dict(os.environ),os.getcwd()
 for mode,prog in enumerate(('codex-session-relay','crw relay')):
  root.prog=prog
  for name,child in children.items(): child.prog=prog+' '+name
  for label,template in cases:
   home=str(pathlib.Path(request['home'])/str(len(results)))
   pathlib.Path(home).mkdir(parents=True)
   os.environ.update(HOME=home,XDG_STATE_HOME=home+'/state',XDG_CONFIG_HOME=home+'/config',XDG_DATA_HOME=home+'/data',XDG_CACHE_HOME=home+'/cache',CODEX_HOME=home+'/codex')
   os.chdir(home)
   argv=[a.replace('{home}',home) for a in template]
   out,err=io.StringIO(),io.StringIO();code=0
   with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):
    try:code=main(argv)
    except SystemExit as e:code=e.code
   results.append(dict(command='region-show' if argv==['region-show','--help'] else '<root>',label=label,args=argv,code=code,out=out.getvalue(),err=err.getvalue(),mode=mode,home=home))
   shutil.rmtree(home)
 os.chdir(cwd);os.environ.clear();os.environ.update(environ)
for command in request['commands']:
 p=children[command]
 actions=[a for a in p._actions if a.option_strings and a.dest!='help']
 def value(a):
  if a.choices: return str(next(iter(a.choices)))
  return '1' if a.type in (int,float) else 'x'
 required=[a for a in actions if a.required]
 for g in p._mutually_exclusive_groups:
  if g.required: required.append(g._group_actions[0])
 def args_for(items):
  return [v for a in items for v in ([a.option_strings[-1]] if a.nargs==0 else [a.option_strings[-1],value(a)])]
 base=args_for(required)
 cases=[('help',['--help']),('none',[]),('unknown',base+['--unknown']),('unknown-equals',base+['--unknown=x']),('positional',base+['x']),('short',base+['-x']),('end',base+['--']),('end-positional',base+['--','x']),('help-unknown',['--help','--unknown']),('help-explicit',['--help=x']),('short-help-explicit',['-hx'])]
 for a in required: cases.append(('drop-'+a.dest,args_for([b for b in required if b is not a])))
 allflags=['--help']+[f for a in actions for f in a.option_strings]
 prefixes={f[:i] for f in allflags for i in range(3,len(f))}
 for prefix in sorted(prefixes):
  matches=[f for f in allflags if f.startswith(prefix)]
  if len(matches)>1 and prefix not in allflags:
   cases.extend([('ambiguous-'+prefix,base+[prefix]),('ambiguous-equals-'+prefix,base+[prefix+'=x']),('help-ambiguous-'+prefix,['--help',prefix])])
 for a in actions:
  flag=a.option_strings[-1]; other=args_for([b for b in required if b is not a])
  if a.nargs==0:
   cases.extend([('boolean-value-'+a.dest,other+[flag+'=x']),('boolean-repeat-'+a.dest,other+[flag,flag])]);continue
  val=value(a)
  if not request.get('runtime'):
   cases.extend([('space-equals-'+a.dest,other+[flag+'=a b'])])
   if a.type in (int,float):
    vals=['9999999999999999999999999','٣','١٢','_1','1_','1__0','1_0',' 2 ','+3','-0','0x1p3','\u20031\u2003','\x1c1','9'*4300,'9'*4301]
    if a.type is float: vals += ['nan','+nan','-NaN','inf','+Infinity','-inf','1e400','1e-400','.5','1.','١_٢.٣','1_e2','1e_2']
    for i,v in enumerate(vals): cases.append(('number-'+a.dest+'-'+str(i),other+[flag+'='+v]))
  cases.extend([('missing-end-'+a.dest,other+[flag]),('missing-mid-'+a.dest,other+[flag,'--help']),('dash-'+a.dest,other+[flag,'-x']),('negative-'+a.dest,other+[flag,'-1']),('negative-decimal-'+a.dest,other+[flag,'-.5']),('negative-dot-'+a.dest,other+[flag,'-1.']),('empty-'+a.dest,other+[flag+'=']),('equals-'+a.dest,other+[flag+'='+val]),('repeated-'+a.dest,other+[flag,val,flag,val])])
  if a.choices: cases.extend([('choice-'+a.dest,other+[flag,'INVALID']),('choice-equals-'+a.dest,other+[flag+'=INVALID'])])
  prefix=next((flag[:i] for i in range(3,len(flag)+1) if sum(f.startswith(flag[:i]) for f in allflags)==1),flag)
  cases.extend([('abbreviation-'+a.dest,other+[prefix,val]),('abbreviation-equals-'+a.dest,other+[prefix+'='+val])])
  if not request.get('runtime'): cases.append(('space-abbreviation-'+a.dest,other+[prefix+'=a b']))
 for mode,prog in enumerate(('codex-session-relay','crw relay')):
  root.prog=prog
  for name,child in children.items(): child.prog=prog+' '+name
  for label,tail in cases:
   argv=['--kind-module','crw_parity_absent',command]+tail
   home=''
   if request.get('runtime'):
    home=str(pathlib.Path(request['home'])/str(len(results)))
    pathlib.Path(home).mkdir(parents=True)
    os.environ.update(HOME=home,XDG_STATE_HOME=home+'/state',XDG_CONFIG_HOME=home+'/config',XDG_DATA_HOME=home+'/data',XDG_CACHE_HOME=home+'/cache',CODEX_HOME=home+'/codex')
    os.chdir(home)
    argv=[command]+tail
    if command=='intent-declare': argv=[command,'--declared-at','2026-01-01T00:00:00+00:00']+tail
   out,err=io.StringIO(),io.StringIO(); code=0
   with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):
    try: code=main(argv)
    except SystemExit as e: code=e.code
   results.append(dict(command=command,label=label,args=argv,code=code,out=out.getvalue(),err=err.getvalue(),mode=mode,home=home))
   if home: shutil.rmtree(home)
json.dump(results,sys.stdout)
