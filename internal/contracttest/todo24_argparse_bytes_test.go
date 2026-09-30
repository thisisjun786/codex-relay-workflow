package contracttest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

type argparseRun struct {
	code           int
	stdout, stderr []byte
}

func todo24Root(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func runArgparseBinary(t *testing.T, dir, program string, env []string, argv ...string) argparseRun {
	t.Helper()
	cmd := exec.Command(program, argv...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return argparseRun{code, stdout.Bytes(), stderr.Bytes()}
}

// pythonArgparse is what the Python CLI (`uv run codex-session-relay`) answered to args in dir
// (recorded, internal/testsupport/pyoracle). after runs once the live Python has answered, to
// undo what it wrote before the Go side runs; a replay has nothing to undo.
func pythonArgparse(t *testing.T, key, dir, root string, env, args []string, after func(), opts ...pyoracle.Option) argparseRun {
	t.Helper()
	answer := pythonProcess(t, key, func() (processAnswer, error) {
		pythonArgs := append([]string{"run", "--no-sync", "--project", root, "codex-session-relay"}, args...)
		run := runArgparseBinary(t, dir, "uv", env, pythonArgs...)
		if after != nil {
			after()
		}
		return processAnswer{exit: run.code, stdout: run.stdout, stderr: run.stderr}, nil
	}, append(opts, pyoracle.Substitute(dir, "<HOME>"), pyoracle.Substitute(root, "<ROOT>"))...)
	return argparseRun{answer.exit, answer.stdout, answer.stderr}
}

func requireArgparseBytes(t *testing.T, want, got argparseRun) {
	t.Helper()
	if want.code != got.code || !bytes.Equal(want.stdout, got.stdout) || !bytes.Equal(want.stderr, got.stderr) {
		t.Fatalf("Python exit=%d stdout=%q stderr=%q\nGo exit=%d stdout=%q stderr=%q", want.code, want.stdout, want.stderr, got.code, got.stdout, got.stderr)
	}
}

func replaceArg(base []string, flag, value string) []string {
	out := append([]string(nil), base...)
	for i := range out {
		if out[i] == flag {
			out[i+1] = value
			return out
		}
	}
	return append(out, flag, value)
}

// The Python side of the two Test24 tests is recorded (internal/testsupport/pyoracle).
func Test24IntentDeclareSamePathMatchesLivePythonBytes(t *testing.T) {
	root := todo24Root(t)
	binary := filepath.Join(t.TempDir(), "codex-session-relay")
	build := exec.Command("go", "build", "-o", binary, "./cmd/crw")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	home := t.TempDir()
	marker, workspace, state := filepath.Join(home, "marker"), filepath.Join(home, "work"), filepath.Join(home, "state")
	if err := os.MkdirAll(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), "XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(home, "xdg-data"), "XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"), "CODEX_HOME="+filepath.Join(home, "codex"), "COLUMNS=80", "TMPDIR="+os.TempDir())
	args := []string{"--state", state, "intent-declare", "--marker-root", marker, "--workspace", workspace, "--dispatch-request-id", "D", "--issue", "I", "--declared-at", "2026-01-01T00:00:00+00:00"}
	// The assignment directory is named by the workspace's digest, which follows the temporary
	// path; the recording names it by placeholder, and a replay puts back this run's digest.
	workspaceKey, err := delivery.WorkspaceKey(workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := pythonArgparse(t, "python", home, root, env, args, func() {
		if err := os.RemoveAll(marker); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(state); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(marker, 0700); err != nil {
			t.Fatal(err)
		}
	}, pyoracle.Substitute(workspaceKey, "<WORKSPACE_KEY>"))
	got := runArgparseBinary(t, home, binary, env, args...)
	requireArgparseBytes(t, want, got)
}

func Test24Todo24ArgparseSweepMatchesLivePythonBytes(t *testing.T) {
	root := todo24Root(t)
	binary := filepath.Join(t.TempDir(), "codex-session-relay")
	build := exec.Command("go", "build", "-o", binary, "./cmd/crw")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	type command struct {
		name               string
		base, abbreviation []string
		first              string
		ambiguous          []string
		badChoice          [2]string
	}
	commands := []command{
		{"intent-declare", []string{"--workspace", "W", "--dispatch-request-id", "D", "--issue", "I"}, []string{"--work", "W", "--dispatch-r", "D", "--iss", "I"}, "--workspace", []string{"--d", "x"}, [2]string{}},
		{"intent-attempt", []string{"--workspace", "W", "--assignment", "A", "--outcome", "accepted"}, []string{"--work", "W", "--ass", "A", "--out", "accepted"}, "--workspace", nil, [2]string{"--outcome", "bad"}},
		{"intent-bind", []string{"--workspace", "W", "--assignment", "A", "--session", "S", "--task-id", "T"}, []string{"--work", "W", "--ass", "A", "--sess", "S", "--task", "T"}, "--workspace", nil, [2]string{}},
		{"intent-register", []string{"--workspace", "W", "--assignment", "A", "--relationship", "R", "--dispatch-request-id", "D"}, []string{"--work", "W", "--ass", "A", "--rel", "R", "--dispatch-r", "D"}, "--workspace", []string{"--d", "x"}, [2]string{}},
		{"intent-claim", []string{"--workspace", "W", "--assignment", "A", "--session", "S", "--dispatch-request-id", "D"}, []string{"--work", "W", "--ass", "A", "--sess", "S", "--dispatch-r", "D"}, "--workspace", []string{"--d", "x"}, [2]string{}},
		{"intent-disposition", []string{"--workspace", "W", "--assignment", "A", "--session", "S", "--turn", "T", "--outcome", "failed"}, []string{"--work", "W", "--ass", "A", "--sess", "S", "--turn", "T", "--out", "failed"}, "--workspace", nil, [2]string{"--outcome", "bad"}},
		{"intent-resolve", []string{"--workspace", "W", "--assignment", "A", "--chosen-task", "T", "--chosen-session", "S", "--reason", "R", "--adjudicate", "F=D"}, []string{"--work", "W", "--ass", "A", "--chosen-t", "T", "--chosen-s", "S", "--rea", "R", "--adj", "F=D"}, "--workspace", []string{"--chosen", "x"}, [2]string{}},
		{"intent-show", []string{"--workspace", "W"}, []string{"--work", "W"}, "--workspace", nil, [2]string{}},
		{"linkage-directive", []string{"--scope-kind", "project", "--scope", "S", "--from-task", "F", "--from-scope", "FS", "--link", "L", "--digest", "D"}, []string{"--scope-k", "project", "--scope", "S", "--from-t", "F", "--from-s", "FS", "--li", "L", "--di", "D"}, "--scope", []string{"--s", "x"}, [2]string{"--scope-kind", "bad"}},
		{"merge-evidence", []string{"--repository", "owner/repo", "--pull-request", "7"}, []string{"--repo", "owner/repo", "--pull", "7"}, "--repository", []string{"--rest", "x"}, [2]string{"--pull-request", "bad"}},
	}
	for _, command := range commands {
		cases := map[string][]string{
			"help":          {"--help"},
			"missing":       {},
			"unknown":       {"--bogus"},
			"extra":         append(append([]string(nil), command.base...), "extra"),
			"abbreviation":  append(append([]string(nil), command.abbreviation...), "extra"),
			"missing-value": {command.first},
			"empty-value":   append(replaceArg(command.base, command.first, ""), "extra"),
		}
		if command.ambiguous != nil {
			cases["ambiguity"] = command.ambiguous
		}
		if command.badChoice[0] != "" {
			cases["bad-choice"] = append(replaceArg(command.base, command.badChoice[0], command.badChoice[1]), "extra")
		} else {
			cases["bad-choice"] = append(append([]string(nil), command.base...), "--outcome", "bad")
		}
		for name, argv := range cases {
			t.Run(command.name+"/"+name, func(t *testing.T) {
				home := t.TempDir()
				env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), "XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(home, "xdg-data"), "XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"), "CODEX_HOME="+filepath.Join(home, "codex"), "COLUMNS=80", "TMPDIR="+os.TempDir(), "CRW_FORGE_SCENARIO=ready", "PATH="+filepath.Join(root, "internal/relay/cli/testdata")+":"+os.Getenv("PATH"))
				args := append([]string{command.name}, argv...)
				want := pythonArgparse(t, "python", home, root, env, args, nil)
				got := runArgparseBinary(t, home, binary, env, args...)
				requireArgparseBytes(t, want, got)
			})
		}
	}
}
