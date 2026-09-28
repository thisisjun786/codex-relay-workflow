import argparse
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from codex_session_relay.cli import build_parser

binary=Path(sys.argv[1])
alias=binary.with_name('codex-session-relay')
if not alias.exists():alias.symlink_to(binary)
parser=build_parser()
commands=next(a for a in parser._actions if isinstance(a,argparse._SubParsersAction)).choices
cases=[]
for name in ('emit','deliver','reconcile','recover','claim','ack','verify-acks','managed-start'):
    command=commands[name]
    options=[a for a in command._actions if a.option_strings and a.dest!='help']
    base=[]
    for action in options:
        if action.required:
            value=action.choices[0] if action.choices else '1' if action.type is int else 'value'
            base.extend((action.option_strings[0],str(value)))
    cases.extend(([name,'--help'],[name,'-h'],[name],[name,*base,'--unknown'],[name,*base,'--unknown=x'],[name,*base,'extra'],[name,*base,'--'],[name,*base,'--','x']))
    for action in options:
        flag=action.option_strings[0]
        if action.required:
            reduced=list(base);i=reduced.index(flag);del reduced[i:i+2];cases.append([name,*reduced])
        if action.nargs==0:
            cases.append([name,*base,flag+'=value'])
        else:
            cases.extend(([name,*base,flag],[name,*base,flag,'-value'],[name,*base,flag+'='],[name,*base,flag,'first',flag,'second']))
            if action.choices:cases.extend(([name,*base,flag,'bad-choice'],[name,*base,flag+'=bad-choice']))
            for end in range(3,len(flag)):
                prefix=flag[:end]
                matches=[a for a in options if any(option.startswith(prefix) for option in a.option_strings)]
                if len(matches)==1:
                    cases.extend(([name,*base,prefix,'value'],[name,*base,prefix+'=value']));break
            ambiguous=next((flag[:end] for end in range(3,len(flag)) if sum(any(option.startswith(flag[:end]) for option in a.option_strings) for a in options)>1),None)
            if ambiguous:cases.extend(([name,*base,ambiguous,'value'],[name,*base,ambiguous+'=value']))
diffs=[]
for width in (None,'80','120'):
    if width is None:os.environ.pop('COLUMNS',None)
    else:os.environ['COLUMNS']=width
    for program,prefix in ((str(alias),[]),(str(binary),['relay'])):
        for argv in cases:
            expected_out,expected_err=io.StringIO(),io.StringIO()
            p=build_parser();p.prog='codex-session-relay' if not prefix else 'crw relay'
            # argparse captures prog on every child when subparsers are constructed.
            for child in next(a.choices for a in p._actions if isinstance(a,argparse._SubParsersAction)).values():
                child.prog=p.prog+' '+child.prog.rsplit(' ',1)[-1]
            try:
                with contextlib.redirect_stdout(expected_out),contextlib.redirect_stderr(expected_err):p.parse_args(argv)
            except SystemExit as error:code=error.code
            else:continue  # Handler scenarios are covered separately with real stores.
            result=subprocess.run([program,*prefix,*argv],capture_output=True)
            if (result.returncode,result.stdout,result.stderr)!=(code,expected_out.getvalue().encode(),expected_err.getvalue().encode()):
                diffs.append({'width':width,'prefix':prefix,'argv':argv,'python':{'exit':code,'stdout':expected_out.getvalue(),'stderr':expected_err.getvalue()},'go':{'exit':result.returncode,'stdout':result.stdout.decode(),'stderr':result.stderr.decode()}})
print(json.dumps({'cases':len(cases),'differences':len(diffs),'first':diffs[:10]},indent=2))
raise SystemExit(bool(diffs))
