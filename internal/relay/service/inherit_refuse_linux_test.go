//go:build linux

package service

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccompChildEnv makes the child copy of this test binary install a seccomp filter before it runs
// the sweep.
const seccompChildEnv = "CRW_1057_SECCOMP_CHILD"

// denyMarking installs a seccomp filter that fails close_range and fcntl(F_SETFD) with EPERM and
// allows everything else (open, dup3, fcntl(F_GETFD), the directory reads): the sandbox of the
// re-evaluation of 9c0015af, where the descriptors cannot be marked close-on-exec.
func denyMarking() error {
	const (
		offsetNr  = 0  // seccomp_data.nr
		offsetArg = 24 // seccomp_data.args[1], the fcntl command
	)
	eperm := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offsetNr},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLOSE_RANGE, Jt: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_FCNTL, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offsetArg},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.F_SETFD, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: eperm},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	if errno != 0 {
		return errno
	}
	return nil
}

// A start whose descriptors cannot be marked close-on-exec must be refused, not run with the
// caller's descriptor handed on (CRW-1057 re-evaluation of 9c0015af, d1): unix.CloseOnExec returns
// no error, so the sweep reported success from a readable /proc/self/fd while the descriptor kept
// surviving the exec. The sandbox is a seccomp filter that fails close_range and fcntl(F_SETFD).
func TestFDSweepRefusesWhatItCannotMark(t *testing.T) {
	if os.Getenv(seccompChildEnv) != "" {
		fd := heldWithoutCloExec(t)
		if err := denyMarking(); err != nil {
			t.Skipf("no seccomp filter here: %v", err)
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err == nil {
			t.Fatal("the filter does not stop F_SETFD")
		}
		err := defaultFDSweep.apply()
		t.Logf("descriptor %d, close-on-exec %v, sweep error: %v", fd, cloExec(t, fd), err)
		if err == nil {
			t.Fatalf("the sweep reported success while descriptor %d still survives an exec", fd)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFDSweepRefusesWhatItCannotMark$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), seccompChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if strings.Contains(string(out), "--- SKIP") {
		t.Skip("the child could not install a seccomp filter")
	}
	if err != nil {
		t.Fatalf("child failed: %v", err)
	}
}
