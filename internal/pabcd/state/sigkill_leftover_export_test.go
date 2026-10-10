package state

import (
	"os"
	"syscall"
	"time"
)

// SigkillSteps are the points of the session state writes at which SigkillChild kills its own process (CRW-1255), in the order the
// leftover table of sigkill_leftover_test.go lists them.
var SigkillSteps = []string{
	"lock-stage-record", "lock-before-publish", "lock-held", "lock-takeover-record",
	"write-before-sync", "write-before-rename", "write-after-rename",
	"ensure-before-link", "ensure-after-link", "ensure-fallback-sync", "ensure-fallback-publish",
	"ensure-inplace-write", "ensure-inplace-sync",
}

// SigkillChild is the body of the child process of the SIGKILL leftover test: it runs the session state write up to step and then
// kills itself with SIGKILL, so nothing after that point (a deferred removal, a release) runs. A step this function does not know
// is a failed child, not a kill. It lives in this file because the seams it reaches are unexported; the test that drives it is the
// external package, so it can import the reset command that sweeps orphans.
func SigkillChild(step, cwd string) {
	die := func() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL); select {} }
	const id = "conc"
	now := time.Now
	nextState := func() State {
		s := DefaultState(id, "next")
		s.Phase = PhaseC
		return s
	}
	switch step {
	case "lock-stage-record", "lock-takeover-record": // the staged file exists and holds its kernel lock; its record is not written
		sessionLockWriteRecord = func(*os.File, string) error { die(); return nil }
		_ = WithSessionLock(cwd, id, func() error { return nil })
	case "lock-before-publish": // the staged file holds its record; it is not at the path yet
		sessionLockRenameNoReplace = func(string, string) error { die(); return nil }
		_ = WithSessionLock(cwd, id, func() error { return nil })
	case "lock-held": // the holder dies inside its critical section
		_ = WithSessionLock(cwd, id, func() error { die(); return nil })
	case "write-before-sync", "write-before-rename", "write-after-rename":
		rename := func(tmp, final string) error { return os.Rename(tmp, final) }
		sync := (*os.File).Sync
		switch step {
		case "write-before-sync": // the temp file is written, not yet fsynced or closed
			sync = func(f *os.File) error {
				if fi, err := f.Stat(); err == nil && !fi.IsDir() {
					die()
				}
				return f.Sync()
			}
		case "write-before-rename":
			rename = func(string, string) error { die(); return nil }
		case "write-after-rename": // the new state is published; the directory is not fsynced
			sync = func(f *os.File) error {
				if fi, err := f.Stat(); err == nil && fi.IsDir() {
					die()
				}
				return f.Sync()
			}
		}
		_ = WithSessionLock(cwd, id, func() error { return writeState(cwd, nextState(), now(), rename, sync) })
	case "ensure-before-link": // the default state is in the temp file; the hard link is not made
		_, _ = ensureState(cwd, id, now, func(string, string) error { die(); return nil })
	case "ensure-after-link": // the state file is published; the temp name is not removed
		_, _ = ensureState(cwd, id, now, func(tmp, final string) error {
			if err := os.Link(tmp, final); err != nil {
				return err
			}
			die()
			return nil
		})
	case "ensure-fallback-sync", "ensure-fallback-publish": // no hard links: the second temp file is written, then the kill
		want := stepSync
		if step == "ensure-fallback-publish" {
			want = stepPublish
		}
		_, _ = ensureStateWith(cwd, id, now, func(string, string) error { return syscall.EPERM }, func(s ensureStep, _ string) error {
			if s == want {
				die()
			}
			return nil
		})
	case "ensure-inplace-write", "ensure-inplace-sync": // no hard links and no no-replace rename: the final path is created in place, then the kill
		want := stepWrite // before the data: the final path is an empty file
		if step == "ensure-inplace-sync" {
			want = stepSync // after the data is written, before the fsync
		}
		final := StatePath(cwd, id)
		_, _ = ensureStateWith(cwd, id, now, func(string, string) error { return syscall.EPERM }, func(s ensureStep, path string) error {
			switch {
			case s == stepPublish:
				return syscall.ENOSYS // the stand-in for the no-replace rename: unsupported, so the file is created in place
			case s == want && path == final:
				die()
			}
			return nil
		})
	default:
		os.Exit(3)
	}
	os.Exit(4) // the step ran to its end without reaching the kill
}

// SigkillSleepSeam replaces the sleep WithSessionLock waits with and returns the call that puts the previous one back.
func SigkillSleepSeam(sleep func(time.Duration)) (restore func()) {
	previous := sessionLockSleep
	sessionLockSleep = sleep
	return func() { sessionLockSleep = previous }
}
