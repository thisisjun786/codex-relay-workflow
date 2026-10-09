//go:build linux

package job

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// heldWithoutCloExec opens path without O_CLOEXEC and moves it to 20 and up, as a shell's exec 9>lock leaves one (CRW-1081). The number
// is far below the soft descriptor limit, so the test does not depend on a raised RLIMIT_NOFILE.
func heldWithoutCloExec(t *testing.T, path string) int {
	t.Helper()
	opened, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(opened), unix.F_DUPFD, 20)
	_ = unix.Close(opened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatalf("descriptor %d is close-on-exec before the start: %d, %v", fd, flags, err)
	}
	return fd
}

// childDescriptors runs `ls -l /proc/self/fd` as the job and returns what the job's descriptors name, from the job's own output.
func childDescriptors(t *testing.T, ws string, startJob func(*exec.Cmd) error) string {
	t.Helper()
	rec, err := runBackground(ws, RunOptions{Command: []string{"ls", "-l", "/proc/self/fd"}}, time.Now, startJob)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PID != nil {
		pid := *rec.PID
		t.Cleanup(func() { _ = unix.Kill(-pid, unix.SIGKILL) })
	}
	done := settled(t, ws, rec.ID)
	out := get(t, OutPath(ws, rec.ID))
	// A failed listing names no descriptor at all, so "the held path is absent" proves nothing then: the probe must have succeeded and
	// listed the job's own stdout, which is the output file.
	if done.Status != StatusComplete || done.ExitCode == nil || *done.ExitCode != 0 || !strings.Contains(out, OutPath(ws, rec.ID)) {
		t.Fatalf("the descriptor listing of the job did not run: record %+v, output:\n%s", done, out)
	}
	return out
}

func defaultStart(cmd *exec.Cmd) error { return cmd.Start() }

// The caller of `relay job run` may hold a descriptor without close-on-exec (the installer's integration lock on fd 9): the job outlives
// the caller, so it must not carry that descriptor (CRW-1081, the job path of CRW-1057).
func TestRunBackgroundDoesNotInheritCallerDescriptors(t *testing.T) {
	ws := workspace(t)
	held := filepath.Join(t.TempDir(), "integration.lock")
	fd := heldWithoutCloExec(t, held)
	out := childDescriptors(t, ws, defaultStart)
	t.Logf("caller holds %d -> %s; the job sees:\n%s", fd, held, out)
	if strings.Contains(out, held) {
		t.Errorf("the job inherited the caller's descriptor %d -> %s:\n%s", fd, held, out)
	}
}

// A descriptor passed on purpose (ExtraFiles) is still in the job, next to a caller descriptor that is not.
func TestRunBackgroundKeepsExtraFiles(t *testing.T) {
	ws := workspace(t)
	held := filepath.Join(t.TempDir(), "integration.lock")
	heldWithoutCloExec(t, held)
	passed := filepath.Join(t.TempDir(), "passed-on-purpose")
	f, err := os.Create(passed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := childDescriptors(t, ws, func(cmd *exec.Cmd) error {
		cmd.ExtraFiles = []*os.File{f}
		return cmd.Start()
	})
	if !strings.Contains(out, passed) {
		t.Errorf("the descriptor passed on purpose is not in the job:\n%s", out)
	}
	if strings.Contains(out, held) {
		t.Errorf("the job inherited the caller's descriptor -> %s:\n%s", held, out)
	}
}

// A start that cannot keep the caller's descriptors out of the job is refused: nothing is started, no record, no ledger row.
func TestRunBackgroundRefusedWhenTheSweepFails(t *testing.T) {
	refuse := errors.New("F_SETFD refused")
	saved := markInherited
	t.Cleanup(func() { markInherited = saved })
	markInherited = func() error { return refuse }
	ws := workspace(t)
	starts := 0
	_, err := runBackground(ws, RunOptions{Command: []string{"true"}}, time.Now, func(*exec.Cmd) error { starts++; return nil })
	if !errors.Is(err, refuse) || starts != 0 || len(ListRecordIDs(ws)) != 0 || exists(filepath.Join(BGDir(ws), LedgerFile)) {
		t.Fatalf("want the start refused with the failed mark and nothing started or recorded, got %v (started %d times)", err, starts)
	}
}
