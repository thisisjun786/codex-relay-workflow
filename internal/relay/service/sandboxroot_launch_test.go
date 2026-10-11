package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1265. A relay started from the account home (a login shell, a user unit with no
// WorkingDirectory) must not keep that directory as its cwd: the supervisor and the worker run in
// the relay's state directory. Start and SpawnWorker both start their process through launch.
func TestRelayStartedFromHomeRunsInStateDir(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "relay-state")
	t.Chdir(home)
	s := &Service{Selection: store.StateSelection{Path: state}}
	cmd := exec.Command("/bin/sh", "-c", "pwd -P > cwd.out")
	if err := s.launch(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(state, "cwd.out"))
	if err != nil {
		t.Fatalf("the process did not run in the state directory: %v", err)
	}
	want, _ := filepath.EvalSymlinks(state)
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Fatalf("the process ran in %s, want the state directory %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, "cwd.out")); err == nil {
		t.Fatal("the process wrote into the directory the relay was started from")
	}
}
