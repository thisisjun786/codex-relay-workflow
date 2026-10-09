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
		limit:      func() (int, error) { return 0, nil },
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
		limit:      boundedBy(4096),
	}
	if err := sweep.apply(); err != nil {
		t.Fatal(err)
	}
	if !cloExec(t, fd) {
		t.Fatalf("descriptor %d still survives an exec", fd)
	}
}

// boundedBy is the walk bound of a kernel whose maximum descriptor count is kernelMax.
func boundedBy(kernelMax uint64) func() (int, error) {
	return func() (int, error) { return descriptorBound(func() (uint64, error) { return kernelMax, nil }) }
}

// The walk bound is only an answer when it is an upper bound of every open descriptor (CRW-1057
// re-evaluation of 9c0015af): the larger of the limits and the kernel's maximum, or an error.
func TestDescriptorBound(t *testing.T) {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	known := func(n uint64) func() (uint64, error) { return func() (uint64, error) { return n, nil } }
	if n, err := descriptorBound(known(sweepCap)); err != nil || n != sweepCap {
		t.Fatalf("the kernel maximum %d is walkable: got %d, %v", sweepCap, n, err)
	}
	if limit.Max < sweepCap-1000 { // a hard limit near the cap leaves no room for a larger maximum
		small := max(limit.Cur, limit.Max) + 1000
		if n, err := descriptorBound(known(small)); err != nil || uint64(n) != small {
			t.Fatalf("a maximum above both limits is the bound: want %d, got %d, %v", small, n, err)
		}
	}
	if n, err := descriptorBound(known(sweepCap + 1)); !errors.Is(err, ErrDescriptorsUnbounded) {
		t.Fatalf("a maximum above the cap has no bound, got %d, %v", n, err)
	}
	unreadable := func() (uint64, error) { return 0, os.ErrNotExist }
	if n, err := descriptorBound(unreadable); !errors.Is(err, ErrDescriptorsUnbounded) {
		t.Fatalf("an unknown maximum has no bound, got %d, %v", n, err)
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

// heldHigh is the number of the descriptor that stays open above the lowered limits, and kernelHigh
// is the maximum descriptor count of the kernel the child pretends to run on.
const (
	heldHigh   = 90000
	kernelHigh = 100000
)

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

// sweepPastLoweredLimit holds a descriptor at heldHigh, lowers the soft (and with hard, the hard)
// descriptor limit to 64 and runs the last-resort walk.
func sweepPastLoweredLimit(t *testing.T, hard bool) {
	opened, err := unix.Open(filepath.Join(t.TempDir(), "held"), unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(opened), unix.F_DUPFD, heldHigh)
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
		limit:      boundedBy(kernelHigh),
	}
	if err := sweep.apply(); err != nil {
		t.Fatal(err)
	}
	bound, _ := boundedBy(kernelHigh)()
	t.Logf("descriptor %d, limit %d/%d, walk bound %d", fd, limit.Cur, limit.Max, bound)
	if !cloExec(t, fd) {
		t.Fatalf("descriptor %d above the lowered limit still survives an exec", fd)
	}
}
