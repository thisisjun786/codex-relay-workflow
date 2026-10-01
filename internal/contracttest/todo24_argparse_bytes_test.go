package contracttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func todo24Root(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func runArgparseBinary(t *testing.T, dir, program string, env []string, argv ...string) processAnswer {
	t.Helper()
	cmd := exec.Command(program, argv...)
	cmd.Dir, cmd.Env = dir, env
	answer, err := runProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// The built crw answers an intent-declare line with the golden's exit status and output bytes
// (first taken as what the Python CLI, `uv run codex-session-relay`, answered).
func Test24IntentDeclareSamePathMatchesLivePythonBytes(t *testing.T) {
	root := todo24Root(t)
	binary := relayAlias(t)
	home := t.TempDir()
	marker, workspace, state := filepath.Join(home, "marker"), filepath.Join(home, "work"), filepath.Join(home, "state")
	if err := os.MkdirAll(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), "XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(home, "xdg-data"), "XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir())
	args := []string{"--state", state, "intent-declare", "--marker-root", marker, "--workspace", workspace, "--dispatch-request-id", "D", "--issue", "I", "--declared-at", "2026-01-01T00:00:00+00:00"}
	// The assignment directory is named by the workspace's digest, which follows the temporary
	// path; the golden names it by placeholder, and a check puts back this run's digest.
	workspaceKey, err := delivery.WorkspaceKey(workspace)
	if err != nil {
		t.Fatal(err)
	}
	got := runArgparseBinary(t, home, binary, env, args...)
	checkProcess(t, "answer", got, golden.Substitute(workspaceKey, "<WORKSPACE_KEY>"), golden.Substitute(home, "<HOME>"), golden.Substitute(root, "<ROOT>"))
}

// relayAlias is the crw under test spelled codex-session-relay, the name its usage text
// carries.
func relayAlias(t *testing.T) string {
	t.Helper()
	built, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "codex-session-relay")
	if err := os.Symlink(built, alias); err != nil {
		t.Fatal(err)
	}
	return alias
}
