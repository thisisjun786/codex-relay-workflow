package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const helloScript = "../../../testdata/flow/hello.js"

func runFlow(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunCLI(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// crw flow run --backend fake testdata/flow/hello.js prints the same bytes on every run.
func TestFlowRunHelloIsRepeatable(t *testing.T) {
	var first string
	for i := 0; i < 5; i++ {
		code, out, errOut := runFlow(t, "run", "--backend", "fake", helloScript)
		if code != 0 || errOut != "" {
			t.Fatalf("run %d: exit %d %q", i, code, errOut)
		}
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("run %d printed different bytes", i)
		}
	}
	var res Result
	if err := json.Unmarshal([]byte(first), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != RunCompleted || res.Workflow != "hello" || len(res.Calls) != 7 || len(res.Phases) != 2 || len(res.Logs) != 1 {
		t.Errorf("%+v", res)
	}
	if !strings.Contains(first, `"path": "root/2:1"`) || strings.Contains(first, "startSeq") {
		t.Errorf("output %s", first)
	}
}

func TestFlowRunUsageAndErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.js", "export const meta = { name: 'ok' }\nreturn args\n")
	bad := write("bad.js", "export const meta = { name: 'bad' }\nthrow new Error('nope')\n")
	badLoad := write("badload.js", "const a = 1\n")
	for name, c := range map[string]struct {
		args []string
		code int
		out  string
		err  string
	}{
		"no command":      {nil, 2, "", "required: command"},
		"unknown command": {[]string{"start"}, 2, "", "invalid choice"},
		"help":            {[]string{"--help"}, 0, "usage: crw flow run", ""},
		"no backend":      {[]string{"run", ok}, 2, "", "--backend is required"},
		"other backend":   {[]string{"run", "--backend", "appserver", ok}, 2, "", "not available in this build"},
		"no script":       {[]string{"run", "--backend", "fake"}, 2, "", "exactly one script file"},
		"missing script":  {[]string{"run", "--backend", "fake", filepath.Join(dir, "none.js")}, 2, "", "no such file"},
		"bad args":        {[]string{"run", "--backend", "fake", "--args", "{", ok}, 2, "", "not valid JSON"},
		"args twice":      {[]string{"run", "--backend", "fake", "--args", "1", "--args-file", ok, ok}, 2, "", "exclude each other"},
		"args":            {[]string{"run", "--backend", "fake", ok, "--args", `{"k":[1,2]}`}, 0, `"k": [`, ""},
		"script error":    {[]string{"run", "--backend", "fake", bad}, 1, `"errorKind": "js_error"`, ""},
		"load error":      {[]string{"run", "--backend", "fake", badLoad}, 1, "", "the header holds imports"},
		"token budget":    {[]string{"run", "--backend", "fake", "--token-budget", "5", "--pool", "2", ok}, 0, `"tokensSpent": 0`, ""},
		"journal":         {[]string{"run", "--backend", "fake", "--journal", ok}, 0, `"journal": [`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runFlow(t, c.args...)
			if code != c.code || !strings.Contains(out, c.out) || !strings.Contains(errOut, c.err) {
				t.Errorf("exit %d (want %d)\nstdout %q\nstderr %q", code, c.code, out, errOut)
			}
		})
	}
}

// A helper that reaches outside the script's directory through a symlink is refused, like one that names ../.
func TestFlowRunHelperCannotEscapeThroughSymlink(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	dir := filepath.Join(base, "wf")
	for _, d := range []string{outside, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.js"), []byte("export const v = 'secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip("no symlinks here")
	}
	p := filepath.Join(dir, "s.js")
	if err := os.WriteFile(p, []byte("import { v } from './link/secret.js'\nexport const meta = { name: 's' }\nreturn v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runFlow(t, "run", "--backend", "fake", p)
	if code != 1 || strings.Contains(out, "secret") || !strings.Contains(errOut, "was not found") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestFlowRunWorkflowFromSibling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kid.js"), []byte("export const meta = { name: 'kid' }\nreturn args.x * 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "main.js")
	if err := os.WriteFile(p, []byte("export const meta = { name: 'main' }\nreturn await workflow('kid', { x: 21 })\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runFlow(t, "run", "--backend", "fake", p)
	if code != 0 || !strings.Contains(out, `"value": 42`) {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// crw flow recover CALL (--adopt ID | --absent): the command line is checked before anything is read, a build without a
// stored journal says so, and with a journal and a host the result is printed (exit 0) or the missing proof is told (exit 1).
func TestFlowRecoverCommandLine(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		code int
		err  string
	}{
		"no call":        {[]string{"recover", "--absent"}, 2, "exactly one call id"},
		"two calls":      {[]string{"recover", "c1", "c2", "--absent"}, 2, "exactly one call id"},
		"no mode":        {[]string{"recover", "c1"}, 2, "exactly one of --adopt and --absent"},
		"both modes":     {[]string{"recover", "c1", "--absent", "--adopt", "t1"}, 2, "exactly one of --adopt and --absent"},
		"no journal":     {[]string{"recover", "c1", "--absent"}, 2, "keeps a run's journal in memory"},
		"flags after":    {[]string{"recover", "c1", "--adopt", "t1"}, 2, "keeps a run's journal in memory"},
		"unknown flag":   {[]string{"recover", "c1", "--force"}, 2, "flag provided but not defined"},
		"help":           {[]string{"recover", "--help"}, 0, "usage: crw flow recover"},
		"top-level help": {[]string{"--help"}, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runFlow(t, c.args...)
			if code != c.code || !strings.Contains(errOut+out, c.err) {
				t.Errorf("exit %d (want %d)\nstdout %q\nstderr %q", code, c.code, out, errOut)
			}
		})
	}
	if _, out, _ := runFlow(t, "--help"); !strings.Contains(out, "crw flow recover") {
		t.Errorf("the usage does not list recover: %q", out)
	}
	t.Run("with a journal and a host", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		host := &stubHost{threads: []HostThread{{ID: "t-new", Cwd: "/w/a1", Source: "appServer", CreatedAt: time.UnixMilli(1000), Status: "idle"}}}
		RecoverEnv = func(ctx context.Context, runID string) (Journal, ReconHost, error) { return j, host, nil }
		defer func() { RecoverEnv = nil }()
		code, out, errOut := runFlow(t, "recover", "--run", "r", "c1", "--absent")
		if code != 0 || !strings.Contains(out, `"action": "thread_ready"`) || !strings.Contains(out, `"threadId": "t-new"`) {
			t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
		}
		// nothing is held any more: the same command is refused with exit 1
		code, _, errOut = runFlow(t, "recover", "--run", "r", "c1", "--absent")
		if code != 1 || !strings.Contains(errOut, "not in recovery_hold") {
			t.Errorf("exit %d stderr %q", code, errOut)
		}
	})
}
