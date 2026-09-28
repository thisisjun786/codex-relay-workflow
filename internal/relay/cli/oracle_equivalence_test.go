//go:build parity

package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The default suite keeps exhaustive matrices behind -tags parity. Before doing so,
// prove that the in-process parser oracle used by the sweep has the same bytes and
// exit status as a fresh console process across 500 generated cases.
func Test24InProcessOracleMatchesConsoleSample(t *testing.T) {
	root := repositoryRoot(t)
	python := filepath.Join(root, ".venv/bin/python")
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "CODEX_HOME="+filepath.Join(home, "codex"), "COLUMNS=80")
	inProcess := `import contextlib,io,json,os,sys
from codex_session_relay.cli import build_parser
cases=json.loads(sys.stdin.read()); out=[]
for argv in cases:
 o,e=io.StringIO(),io.StringIO(); code=0
 try:
  with contextlib.redirect_stdout(o),contextlib.redirect_stderr(e): build_parser().parse_args(argv)
 except SystemExit as x: code=x.code
 out.append({'code':code,'out':o.getvalue(),'err':e.getvalue()})
print(json.dumps(out,separators=(',',':')))`
	console := `import json,os,subprocess,sys
cases=json.loads(sys.stdin.read()); out=[]
script='from codex_session_relay.cli import build_parser; build_parser().parse_args(__import__("json").loads(__import__("sys").argv[1]))'
for argv in cases:
 p=subprocess.run([sys.executable,'-c',script,json.dumps(argv)],capture_output=True,text=True,env=os.environ)
 out.append({'code':p.returncode,'out':p.stdout,'err':p.stderr})
print(json.dumps(out,separators=(',',':')))`
	cases := make([][]string, 0, 500)
	commands := []string{"intent-declare", "reporting-show", "supervisor-stage", "merge-evidence"}
	values := []string{"", "-1", "-.5", "٣", "x", "a b", "é", "--unknown"}
	for len(cases) < 500 {
		i := len(cases)
		command, value := commands[i%len(commands)], values[(i/len(commands))%len(values)]
		cases = append(cases, []string{command, fmt.Sprintf("--unknown-%d=%s", i, value)})
	}
	raw, _ := json.Marshal(cases)
	got := runParityProcessInput(t, env, string(raw), python, "-c", inProcess)
	want := runParityProcessInput(t, env, string(raw), python, "-c", console)
	if got.code != 0 || want.code != 0 || got.err != "" || want.err != "" {
		t.Fatalf("oracle harness failed: in-process=%+v console=%+v", got, want)
	}
	var a, b []any
	if err := json.Unmarshal([]byte(got.out), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want.out), &b); err != nil {
		t.Fatal(err)
	}
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	if string(aJSON) != string(bJSON) {
		t.Fatalf("500-case in-process oracle differs from console")
	}
}
