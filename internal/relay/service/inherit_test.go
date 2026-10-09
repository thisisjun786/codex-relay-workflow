package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

// upTo is a walk that covers n descriptor numbers.
func upTo(n int) func() (int, error) { return func() (int, error) { return n, nil } }

// A descriptor that cannot be marked close-on-exec fails the sweep on every way that finds it, and
// a number that is not open is no failure.
func TestFDSweepFailsWhenAMarkFails(t *testing.T) {
	refuse := errors.New("F_SETFD refused")
	missing := filepath.Join(t.TempDir(), "missing")
	for name, sweep := range map[string]fdSweep{
		"listing": {dirs: []string{missing, t.TempDir()}, limit: upTo(0)},
		"walk":    {dirs: []string{missing}, limit: upTo(8)},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "listing" {
				if err := os.WriteFile(filepath.Join(sweep.dirs[1], "7"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sweep.mark = func(fd int) error {
				if fd == 7 {
					return refuse
				}
				return nil
			}
			if err := sweep.apply(); !errors.Is(err, refuse) {
				t.Fatalf("want the refused mark, got %v", err)
			}
			sweep.mark = func(int) error { return nil }
			if err := sweep.apply(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := (fdSweep{dirs: []string{missing}}).apply(); !errors.Is(err, ErrDescriptorsUnbounded) {
		t.Fatalf("no way and no bound must fail, got %v", err)
	}
}

// markCloseOnExec ignores a number that is not open and marks one that is.
func TestMarkCloseOnExec(t *testing.T) {
	if err := markCloseOnExec(1 << 20); err != nil {
		t.Fatalf("a closed number is not a failure: %v", err)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd := int(f.Fd()) // Fd leaves the descriptor as the runtime had it: close-on-exec
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	if err := markCloseOnExec(fd); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor %d not close-on-exec: %d, %v", fd, flags, err)
	}
}

// A launch whose sweep fails starts nothing: the daemon must not receive the caller's descriptor.
func TestLaunchRefusedWhenTheSweepFails(t *testing.T) {
	refuse := errors.New("F_SETFD refused")
	saved := defaultFDSweep
	t.Cleanup(func() { defaultFDSweep = saved })
	defaultFDSweep = fdSweep{closeRange: func() error { return errors.ErrUnsupported }, dirs: []string{"/dev/null/none"}, limit: upTo(5), mark: func(fd int) error {
		if fd == 4 {
			return refuse
		}
		return nil
	}}
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
