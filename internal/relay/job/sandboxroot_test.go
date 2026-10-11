package job

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. A relay job is an API that runs an arbitrary shell command, not a Codex start: it does not
// limit the workspace. A job whose workspace is the account home, and a job in an allowed workspace
// that changes into the home, keep running; only a sandboxed Codex they might start is judged, where
// it is started (internal/sandboxroot). spawn.go carries no sandbox rule.
func TestJobSpawnRunsNonCodexCommandInHome(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	for name, c := range map[string]struct {
		ws, want string
		argv     []string
	}{
		"workspace is the home":       {w.Home, w.Home, []string{"sh", "-c", "pwd -P"}},
		"cd into the home":            {run, w.Home, []string{"sh", "-c", "cd '" + w.Home + "' && pwd -P"}},
		"workspace above the home":    {filepath.Dir(w.Home), filepath.Dir(w.Home), []string{"sh", "-c", "pwd -P"}},
		"allowed workspace, no moves": {run, run, []string{"sh", "-c", "pwd -P"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := start(t, c.ws, RunOptions{Command: c.argv})
			got := settled(t, c.ws, rec.ID)
			if got.ExitCode == nil || *got.ExitCode != 0 {
				t.Fatalf("the job did not run to a zero exit: %+v", got)
			}
			want, err := filepath.EvalSymlinks(c.want)
			if err != nil {
				t.Fatal(err)
			}
			if out := strings.TrimSpace(get(t, OutPath(c.ws, rec.ID))); out != want {
				t.Fatalf("the job ran in %q, want %q", out, want)
			}
		})
	}
}
