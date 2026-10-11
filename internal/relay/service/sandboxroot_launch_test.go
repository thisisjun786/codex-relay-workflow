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

// CRW-1265. The child runs in the state directory, so a socket path the caller gave relative to its own
// directory is handed over absolute, naming the file the caller meant; a state path that is not
// absolute leaves the child in the caller's directory and the socket as it was given.
func TestCommandKeepsTheCallersRelativeSocket(t *testing.T) {
	start := t.TempDir()
	t.Chdir(start)
	want, err := filepath.Abs("./app.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketOf := func(s *Service) string {
		cmd, err := s.command("service", "run")
		if err != nil {
			t.Fatal(err)
		}
		for i, arg := range cmd.Args {
			if arg == "--socket" {
				return cmd.Args[i+1]
			}
		}
		t.Fatalf("no --socket in %v", cmd.Args)
		return ""
	}
	if got := socketOf(&Service{Selection: store.StateSelection{Path: filepath.Join(t.TempDir(), "state")}, Socket: "./app.sock"}); got != want {
		t.Fatalf("the child got socket %q, want %q", got, want)
	}
	if got := socketOf(&Service{Selection: store.StateSelection{Path: "state"}, Socket: "./app.sock"}); got != "./app.sock" {
		t.Fatalf("a child left in the caller's directory got socket %q", got)
	}
}
