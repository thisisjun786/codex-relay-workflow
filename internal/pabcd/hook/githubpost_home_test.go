package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-1170: githubPostTempHome must not observe the account's real ~/.codex or ~/.crw. Another process of the host
// (a live Codex session opening logs_2.sqlite) creates files there at any moment, and the guard under test wrote
// nothing. The case plays that process: it makes HOME name a directory with a .codex in it, runs the helper in a
// subtest, and has "the host" create the sqlite side files during the subtest. The subtest must pass.
func TestGitHubPostTempHomeIgnoresWhatOtherProcessesWriteToTheRealHome(t *testing.T) {
	realHome := t.TempDir()
	codex := filepath.Join(realHome, ".codex")
	if err := os.MkdirAll(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", realHome)
	passed := t.Run("guard run", func(t *testing.T) {
		githubPostTempHome(t)
		for _, name := range []string{"logs_2.sqlite-shm", "logs_2.sqlite-wal"} {
			if err := os.WriteFile(filepath.Join(codex, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if !passed {
		t.Fatal("a file another process created in the real Codex home failed the guard's test")
	}
}
