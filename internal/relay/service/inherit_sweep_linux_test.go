//go:build linux

package service

import (
	"errors"
	"os"
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
			fd := heldWithoutCloExec(t)
			if name == "close_range" && closeRangeCloExec() != nil {
				t.Skip("this kernel has no close_range with CLOSE_RANGE_CLOEXEC")
			}
			if err := sweep.apply(); err != nil {
				t.Fatal(err)
			}
			if !cloExec(t, fd) {
				t.Fatalf("descriptor %d still survives an exec", fd)
			}
		})
	}
}
