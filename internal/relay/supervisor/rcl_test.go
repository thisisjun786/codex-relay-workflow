package supervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

type rclAnswer struct {
	code    int
	stdout  []byte
	stderr  []byte
	created bool
}

func runRCL(t *testing.T, binary, home string, argv func(string) []string) rclAnswer {
	t.Helper()
	state := filepath.Join(home, "absent-state")
	args := argv(home)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "$STATE", state)
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), "XDG_DATA_HOME="+filepath.Join(home, "xdg-data"), "XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "CODEX_SESSION_RELAY_STATE=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
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

// compareRCLBytes runs argv through the built Go binary and compares its exit code, stderr, state
// creation and stdout after normalize with the golden.
func compareRCLBytes(t *testing.T, argv func(string) []string, normalize func(raw []byte) []byte) rclAnswer {
	t.Helper()
	built := testsupport.CRW(t)
	binary := filepath.Join(t.TempDir(), "codex-session-relay")
	if err := os.Symlink(built, binary); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	golang := runRCL(t, binary, home, argv)
	// A selection made without --socket is scoped by the default socket under CODEX_HOME, whose
	// digest follows home.
	scope, err := store.SocketScope(filepath.Join(home, "codex", "app-server-control", "app-server-control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, "rcl", map[string]any{"code": golang.code, "stdout": string(normalize(golang.stdout)), "stderr": string(golang.stderr), "created": golang.created}, golden.Substitute(scope, "<default-scope>"), golden.Substitute(home, "<home>"), golden.Substitute(repoRoot(t), "<repo>"))
	return golang
}

func Test24_RCL_1_HelpWholeStdoutBytes(t *testing.T) {
	t.Parallel()
	answer := compareRCLBytes(t, func(string) []string { return []string{"reporting-show", "--help"} }, func(raw []byte) []byte { return raw })
	if answer.code != 0 {
		t.Fatalf("exit %d", answer.code)
	}
}

func Test24_RCL_3_UnmanagedObservationWholeStdoutBytes(t *testing.T) {
	t.Parallel()
	normalizeObservedAt := func(raw []byte) []byte {
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

// doctor's whole stdout is the golden but for the runtime-identity fields decisions.md 31
// documents: Go's trailing runtime block, and ownership.runtime_build, which names the answering
// runtime's build (`crw version`) and becomes one token. The golden began as Python's stdout.
func Test24_RCL_4_DoctorWholeStdoutBytes(t *testing.T) {
	t.Parallel()
	version, err := exec.Command(testsupport.CRW(t), "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	goBuild := strings.TrimSpace(string(version))
	normalizeRuntime := func(raw []byte) []byte {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if _, ok := value["runtime"]; !ok {
			t.Fatalf("Go's doctor names no runtime block (decisions.md 31)\n%s", raw)
		}
		delete(value, "runtime")
		block, _ := value["ownership"].(map[string]any)
		if block == nil || block["runtime_build"] != goBuild {
			t.Fatalf("ownership.runtime_build must name the answering runtime's build %q\n%s", goBuild, raw)
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
