package supervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

type rclAnswer struct {
	code    int
	stdout  []byte
	stderr  []byte
	created bool
}

func runRCL(t *testing.T, python bool, binary, home string, argv func(string) []string) rclAnswer {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "absent-state")
	args := argv(home)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "$STATE", state)
	}
	var cmd *exec.Cmd
	if python {
		cmd = exec.Command("uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, args...)...)
		cmd.Dir = filepath.Join(repo, "packages", "codex-session-relay")
	} else {
		cmd = exec.Command(binary, args...)
	}
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), "XDG_DATA_HOME="+filepath.Join(home, "xdg-data"), "XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "CODEX_SESSION_RELAY_STATE=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	_, statErr := os.Stat(state)
	return rclAnswer{code: code, stdout: stdout.Bytes(), stderr: stderr.Bytes(), created: statErr == nil}
}

// compareRCLBytes runs argv through live Python and the built Go binary with one home and
// compares exit codes, stderr, state creation and stdout after normalize, which is told which
// runtime answered.
func compareRCLBytes(t *testing.T, argv func(string) []string, normalize func(raw []byte, python bool) []byte) rclAnswer {
	t.Helper()
	built := supervisorBinary(t)
	binary := filepath.Join(t.TempDir(), "codex-session-relay")
	if err := os.Symlink(built, binary); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	python := runRCL(t, true, binary, home, argv)
	golang := runRCL(t, false, binary, home, argv)
	goOut, pyOut := normalize(golang.stdout, false), normalize(python.stdout, true)
	if golang.code != python.code || !bytes.Equal(goOut, pyOut) || !bytes.Equal(golang.stderr, python.stderr) || golang.created != python.created {
		t.Fatalf("Go exit=%d created=%t\nstdout=%q\nstderr=%q\nPython exit=%d created=%t\nstdout=%q\nstderr=%q", golang.code, golang.created, goOut, golang.stderr, python.code, python.created, pyOut, python.stderr)
	}
	return golang
}

func Test24_RCL_1_HelpWholeStdoutBytes(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	answer := compareRCLBytes(t, func(string) []string { return []string{"reporting-show", "--help"} }, func(raw []byte, _ bool) []byte { return raw })
	if answer.code != 0 {
		t.Fatalf("exit %d", answer.code)
	}
}

func Test24_RCL_3_UnmanagedObservationWholeStdoutBytes(t *testing.T) {
	normalizeObservedAt := func(raw []byte, _ bool) []byte {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		value["observedAt"] = "<injected at process boundary>"
		answer, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(answer, '\n')
	}
	answer := compareRCLBytes(t, func(home string) []string {
		mustMkdirRCL(t, filepath.Join(home, "markers"))
		mustMkdirRCL(t, filepath.Join(home, "workspace"))
		return []string{"--state", "$STATE", "reporting-show", "--marker-root", filepath.Join(home, "markers"), "--workspace", filepath.Join(home, "workspace"), "--assignment", strings.Repeat("a", 64), "--session", "session-1", "--turn", "turn-1"}
	}, normalizeObservedAt)
	if answer.code != 0 || answer.created {
		t.Fatalf("exit=%d state-created=%t", answer.code, answer.created)
	}
}

// doctor's whole stdout is Python's but for the two runtime-identity fields decisions.md 31
// documents: Go's trailing runtime block, and ownership.runtime_build, which names the
// answering runtime. Each side's runtime_build is pinned to its own build (the fence's
// pinned build for Python, `crw version` for Go) before both become one token.
func Test24_RCL_4_DoctorWholeStdoutBytes(t *testing.T) {
	version, err := exec.Command(supervisorBinary(t), "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	goBuild := strings.TrimSpace(string(version))
	normalizeRuntime := func(raw []byte, python bool) []byte {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if _, goOnly := value["runtime"]; goOnly == python {
			t.Fatalf("python=%t: the runtime block is Go's alone (decisions.md 31)\n%s", python, raw)
		}
		delete(value, "runtime")
		block, _ := value["ownership"].(map[string]any)
		want := goBuild
		if python {
			want = ownership.PythonBuild
		}
		if block == nil || block["runtime_build"] != want {
			t.Fatalf("python=%t: ownership.runtime_build must name the answering runtime's build %q\n%s", python, want, raw)
		}
		block["runtime_build"] = "<answering runtime build>"
		answer, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(answer, '\n')
	}
	answer := compareRCLBytes(t, func(string) []string { return []string{"doctor"} }, normalizeRuntime)
	if answer.code != 0 || answer.created {
		t.Fatalf("exit=%d state-created=%t", answer.code, answer.created)
	}
}

func mustMkdirRCL(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}
