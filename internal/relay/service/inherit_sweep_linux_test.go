//go:build linux

package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// heldWithoutCloExec opens a file without O_CLOEXEC and moves it to 20 and up, as a shell's exec 9>
// leaves one.
func heldWithoutCloExec(t *testing.T) int {
	t.Helper()
	opened, err := unix.Open(filepath.Join(t.TempDir(), "held"), unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(opened), unix.F_DUPFD, 20)
	_ = unix.Close(opened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if cloExec(t, fd) {
		t.Fatalf("descriptor %d is close-on-exec before any sweep", fd)
	}
	return fd
}

func cloExec(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	return flags&unix.FD_CLOEXEC != 0
}

var errNoCloseRange = errors.New("close_range not offered")

// A Linux sandbox may leave /proc but hide /dev/fd (CRW-1057 verification): the sweep must neither
// fail the start nor skip the descriptors.
func TestFDSweepWithoutDevFD(t *testing.T) {
	fd := heldWithoutCloExec(t)
	sweep := fdSweep{
		closeRange: func() error { return errNoCloseRange },
		dirs:       []string{filepath.Join(t.TempDir(), "no-dev-fd"), "/proc/self/fd"},
		limit:      func() int { return 0 },
	}
	if err := sweep.apply(); err != nil {
		t.Fatal(err)
	}
	if !cloExec(t, fd) {
		t.Fatalf("descriptor %d still survives an exec", fd)
	}
}

// With no close_range and neither listing, the sweep walks the descriptor numbers.
func TestFDSweepWithoutAnyListing(t *testing.T) {
	fd := heldWithoutCloExec(t)
	missing := filepath.Join(t.TempDir(), "missing")
	sweep := fdSweep{
		closeRange: func() error { return errNoCloseRange },
		dirs:       []string{missing, filepath.Join(missing, "again")},
		limit:      descriptorLimit,
	}
	if err := sweep.apply(); err != nil {
		t.Fatal(err)
	}
	if !cloExec(t, fd) {
		t.Fatalf("descriptor %d still survives an exec", fd)
	}
}

// Every way on its own does the job, and the default sweep does it on this host.
func TestFDSweepEachWay(t *testing.T) {
	ways := map[string]fdSweep{
		"close_range": {closeRange: closeRangeCloExec},
		"proc":        {dirs: []string{"/proc/self/fd"}},
		"default":     defaultFDSweep,
	}
	if _, err := os.Stat("/dev/fd"); err == nil {
		ways["dev"] = fdSweep{dirs: []string{"/dev/fd"}}
	}
	for name, sweep := range ways {
		t.Run(name, func(t *testing.T) {
			// Probe close_range before the descriptor exists: the probe itself marks every open one.
			if name == "close_range" && closeRangeCloExec() != nil {
				t.Skip("this kernel has no close_range with CLOSE_RANGE_CLOEXEC")
			}
			fd := heldWithoutCloExec(t)
			if err := sweep.apply(); err != nil {
				t.Fatal(err)
			}
			if !cloExec(t, fd) {
				t.Fatalf("descriptor %d still survives an exec", fd)
			}
		})
	}
}

// sweepChildEnv names which limits the child of TestFDSweepWalksPastLoweredLimits lowers.
const sweepChildEnv = "CRW_1057_SWEEP_CHILD"

// Lowering RLIMIT_NOFILE does not close the descriptors already open above it, and a caller may
// lower the limit before it runs `relay service start` (CRW-1057 verification of 794b8188). With no
// close_range and neither listing, the walk must still reach such a descriptor, whether the soft
// limit alone or the hard limit too was lowered. Lowering the hard limit cannot be undone, so each
// case runs in a child copy of this test binary.
func TestFDSweepWalksPastLoweredLimits(t *testing.T) {
	if which := os.Getenv(sweepChildEnv); which != "" {
		sweepPastLoweredLimit(t, which == "hard")
		return
	}
	for _, which := range []string{"soft", "hard"} {
		t.Run(which, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFDSweepWalksPastLoweredLimits$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), sweepChildEnv+"="+which)
			out, err := cmd.CombinedOutput()
			t.Logf("child (%s limit lowered):\n%s", which, out)
			if err != nil {
				t.Fatalf("child failed: %v", err)
			}
		})
	}
}

// sweepPastLoweredLimit holds a descriptor at 1000, lowers the soft (and with hard, the hard)
// descriptor limit to 64 and runs the last-resort walk.
func sweepPastLoweredLimit(t *testing.T, hard bool) {
	opened, err := unix.Open(filepath.Join(t.TempDir(), "held"), unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(opened), unix.F_DUPFD, 1000)
	_ = unix.Close(opened)
	if err != nil {
		t.Fatal(err)
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = 64
	if hard {
		limit.Max = 64
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	sweep := fdSweep{
		closeRange: func() error { return errNoCloseRange },
		dirs:       []string{missing, filepath.Join(missing, "again")},
		limit:      descriptorLimit,
	}
	if err := sweep.apply(); err != nil {
		t.Fatal(err)
	}
	t.Logf("descriptor %d, limit %d/%d, walk bound %d", fd, limit.Cur, limit.Max, descriptorLimit())
	if !cloExec(t, fd) {
		t.Fatalf("descriptor %d above the lowered limit still survives an exec", fd)
	}
}
