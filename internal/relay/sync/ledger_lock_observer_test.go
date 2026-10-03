package sync

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// flockLine and otherLine write /proc/locks lines as the kernel prints them (see parseFlockRows). pos is the
// number before the colon: the line's position in the kernel's lock list, not the identity of a lock.
func flockLine(pos int, blocked bool, pid, file string) string {
	arrow := ""
	if blocked {
		arrow = "-> "
	}
	return fmt.Sprintf("%d: %sFLOCK  ADVISORY  WRITE %s %s 0 EOF", pos, arrow, pid, file)
}

func otherLine(pos int, kind, pid, file string) string {
	return fmt.Sprintf("%d: %s ADVISORY  WRITE %s %s 0 EOF", pos, kind, pid, file)
}

func snapshotOf(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

const (
	thisPid  = "4242"
	otherPid = "777"
	// The sidecar's inode number, on the device the holder's lock is listed with and on another device.
	sidecarInode = uint64(2518176)
	sidecarFile  = "fc:00:2518176"
	otherDevice  = "08:01:2518176"
)

func TestObserveSidecar(t *testing.T) {
	cases := []struct {
		name     string
		snapshot string
		held     []string
		blocked  bool
	}{
		{"the holder and the waiter behind it", snapshotOf(
			flockLine(1, false, otherPid, "fc:00:99"), flockLine(2, false, thisPid, sidecarFile), flockLine(2, true, thisPid, sidecarFile)),
			[]string{sidecarFile}, true},
		{"the holder alone", snapshotOf(flockLine(1, false, thisPid, sidecarFile)),
			[]string{sidecarFile}, false},
		{"the holder's line missing", snapshotOf(flockLine(1, false, otherPid, "fc:00:99"), flockLine(2, true, otherPid, "fc:00:99")),
			nil, false},
		{"another process holding and awaiting the same inode number", snapshotOf(
			flockLine(1, false, otherPid, sidecarFile), flockLine(1, true, otherPid, sidecarFile)),
			nil, false},
		{"POSIX and lease lines of this process are no flocks", snapshotOf(
			otherLine(1, "POSIX", thisPid, sidecarFile), otherLine(2, "LEASE", thisPid, sidecarFile), flockLine(3, false, thisPid, sidecarFile)),
			[]string{sidecarFile}, false},
		{"a request blocked on another device's file of that inode number", snapshotOf(
			flockLine(1, false, thisPid, sidecarFile), flockLine(2, false, otherPid, otherDevice), flockLine(2, true, thisPid, otherDevice)),
			[]string{sidecarFile}, false},
		{"two held files of that inode number, the waiter behind the second", snapshotOf(
			flockLine(1, false, thisPid, sidecarFile), flockLine(2, false, thisPid, otherDevice), flockLine(2, true, thisPid, otherDevice)),
			[]string{sidecarFile, otherDevice}, false},
		// One snapshot of the unmodified test's failure run: a read that resumed at a shifted list position
		// printed the holder's line, and the waiter behind it, twice.
		{"the holder's line repeated by a torn read", snapshotOf(
			flockLine(46, false, thisPid, sidecarFile), flockLine(46, true, thisPid, sidecarFile),
			flockLine(47, false, thisPid, sidecarFile), flockLine(47, true, thisPid, sidecarFile)),
			[]string{sidecarFile}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			view := observeSidecar(parseFlockRows(c.snapshot), thisPid, sidecarInode)
			if !slices.Equal(view.held, c.held) || view.blocked != c.blocked {
				t.Errorf("held %v blocked %v, want held %v blocked %v", view.held, view.blocked, c.held, c.blocked)
			}
		})
	}
}

// procSequence answers each snapshot call with the next text, then with the last one for good.
func procSequence(texts ...string) func() ([]lockRow, error) {
	var calls int
	return func() ([]lockRow, error) {
		text := texts[min(calls, len(texts)-1)]
		calls++
		return parseFlockRows(text), nil
	}
}

func TestAwaitBlockedWaiter(t *testing.T) {
	holderOnly := snapshotOf(flockLine(1, false, thisPid, sidecarFile))
	holderAndWaiter := snapshotOf(flockLine(1, false, thisPid, sidecarFile), flockLine(1, true, thisPid, sidecarFile))
	await := func(snapshot func() ([]lockRow, error), bound time.Duration, done <-chan error) error {
		return awaitBlockedWaiter(snapshot, thisPid, sidecarInode, bound, time.Millisecond, done)
	}
	failure := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v, want one containing %q", err, want)
		}
	}
	t.Run("the waiter is seen blocked at once", func(t *testing.T) {
		if err := await(procSequence(holderAndWaiter), 5*time.Second, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a holder's line repeated by a torn read is not a failure", func(t *testing.T) {
		torn := snapshotOf(
			flockLine(46, false, thisPid, sidecarFile), flockLine(46, true, thisPid, sidecarFile),
			flockLine(47, false, thisPid, sidecarFile), flockLine(47, true, thisPid, sidecarFile))
		if err := await(procSequence(torn), 5*time.Second, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a holder's line missing from one snapshot is looked for again", func(t *testing.T) {
		skipped := snapshotOf(flockLine(1, false, otherPid, "fc:00:99"))
		if err := await(procSequence(skipped, skipped, holderAndWaiter), 5*time.Second, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the waiter is seen blocked on a later snapshot", func(t *testing.T) {
		if err := await(procSequence(holderOnly, holderOnly, holderAndWaiter), 5*time.Second, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a waiter that returns first fails", func(t *testing.T) {
		done := make(chan error, 1)
		done <- errors.New("boom")
		failure(t, await(procSequence(holderOnly), 5*time.Second, done), "WithLedgerLock returned (boom)")
	})
	t.Run("no waiter in time fails", func(t *testing.T) {
		failure(t, await(procSequence(holderOnly), 30*time.Millisecond, nil), "no request blocked behind the sidecar lock")
	})
	t.Run("a holder never listed fails and says so", func(t *testing.T) {
		skipped := snapshotOf(flockLine(1, false, otherPid, "fc:00:99"))
		failure(t, await(procSequence(skipped), 30*time.Millisecond, nil), "never listed a flock held by pid 4242 on inode 2518176")
	})
	t.Run("an unreadable /proc/locks fails", func(t *testing.T) {
		unreadable := func() ([]lockRow, error) { return nil, errors.New("read failed") }
		failure(t, await(unreadable, 5*time.Second, nil), "cannot observe the waiter: read failed")
	})
}
