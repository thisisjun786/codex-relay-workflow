import json,os,pathlib,subprocess,sys,tempfile
scenario=sys.argv[1]; restate=sys.argv[2] if len(sys.argv)>2 else None
root=pathlib.Path(__file__).resolve().parents[4]
home=tempfile.mkdtemp()
env=dict(os.environ,HOME=home,XDG_STATE_HOME=home,CODEX_HOME=home,TMPDIR=tempfile.gettempdir(),CRW_FORGE_SCENARIO=scenario)
env["PATH"]=str(root/"internal/relay/cli/testdata")+os.pathsep+env["PATH"]
argv=["uv","run","--no-sync","codex-session-relay","merge-evidence","--repository","owner/repo","--pull-request","7"]
if scenario=="usage":argv=["uv","run","--no-sync","codex-session-relay","merge-evidence","--repository=--x","--pull-request","1"]
if restate:argv += ["--restate",restate]
p=subprocess.run(argv,cwd=root/"packages/codex-session-relay",env=env,text=True,capture_output=True)
print(json.dumps({"code":p.returncode,"payload":json.loads(p.stdout),"stdout":p.stdout,
                  "stderr":p.stderr},sort_keys=True,separators=(",",":")))
