package cli_test

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// inotify is Linux's; this file's name keeps the case out of other targets' builds.

// A read-only form under another owner never opens the write gate (D5: no admission under a
// foreign owner). The fence's lock-free preflight decides before admission would take the
// gate, so a controller's exclusive, non-blocking barrier cannot meet a reader's SH there.
func TestReadOnlyForms_leave_a_foreign_write_gate_unopened(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	state := filepath.Join(home, "python-owned")
	pythonCreates(t, home, state)
	watch, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(watch)
	if _, err := unix.InotifyAddWatch(watch, filepath.Join(state, "write-gate.lock"), unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"status"}, {"store-identity"}, {"fault-show"}, {"store-challenge", "--read", "absent"}} {
		if got := binaryRun(t, alias, append([]string{"--state", state}, argv...)...); got.code != 0 {
			t.Fatalf("%v: %+v", argv, got)
		}
	}
	buffer := make([]byte, 4096)
	if n, err := unix.Read(watch, buffer); n > 0 || !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("the foreign write gate was opened (%d bytes of events, %v)", n, err)
	}
}
