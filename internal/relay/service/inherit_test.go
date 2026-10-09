package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A launch whose sweep fails starts nothing: the daemon must not receive the caller's descriptor.
func TestLaunchRefusedWhenTheSweepFails(t *testing.T) {
	refuse := errors.New("F_SETFD refused")
	saved := closeOnExecInherited
	t.Cleanup(func() { closeOnExecInherited = saved })
	closeOnExecInherited = func() error { return refuse } // the sweep itself is tested in internal/relay/fdsweep
	home := t.TempDir()
	s := &Service{Selection: store.StateSelection{Path: filepath.Join(home, "state")}}
	cmd := exec.Command("/bin/sh", "-c", "touch "+filepath.Join(home, "ran"))
	err := s.launch(cmd)
	if !errors.Is(err, refuse) {
		t.Fatalf("want the start refused with the failed mark, got %v", err)
	}
	if cmd.Process != nil {
		t.Fatalf("the command was started: pid %d", cmd.Process.Pid)
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); err == nil {
		t.Fatal("the command ran")
	}
}
