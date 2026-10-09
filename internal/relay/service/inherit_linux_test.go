//go:build linux

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The 10-09 installer held the integration lock on a descriptor without close-on-exec (exec 9>lock,
// then flock) and ran `relay service start` under it. The supervisor and its worker are long-lived,
// so a descriptor they inherit from that caller outlives the installer and keeps the lock held
// (CRW-1057). The caller here holds such a descriptor across the start: neither daemon may carry it.
func TestServiceStartDoesNotInheritCallerDescriptors(t *testing.T) {
	home := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "integration.lock")
	// A descriptor without O_CLOEXEC, as the shell's exec 9> leaves one. It sits at 20 and up: the
	// launch's ExtraFiles take 3 and up in the child and would overwrite a descriptor there.
	opened, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(opened), unix.F_DUPFD, 20)
	_ = unix.Close(opened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	supervisor, worker := startServing(t, home)
	t.Logf("caller holds %d -> %s", fd, lockPath)
	for _, pid := range []int{supervisor.PID, worker.PID} {
		for _, held := range descriptorTargets(t, pid) {
			t.Logf("process %d fd %s -> %s", pid, held.name, held.target)
			if held.target == lockPath {
				t.Errorf("process %d inherited the caller's descriptor %s -> %s", pid, held.name, held.target)
			}
		}
	}
}

type descriptorTarget struct{ name, target string }

// descriptorTargets lists what each open descriptor of pid names, from /proc/<pid>/fd.
func descriptorTargets(t *testing.T, pid int) []descriptorTarget {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []descriptorTarget
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		out = append(out, descriptorTarget{name: entry.Name(), target: target})
	}
	return out
}
