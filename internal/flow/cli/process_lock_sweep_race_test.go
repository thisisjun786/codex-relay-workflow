package cli

import (
	"os"
	"strings"
	"testing"
)

// CRW-1390, Argon V5 D2 (P3) of 118a891c: a sweep (sweepProcessLocks, the holder of the run lock removing the process lock files nobody
// holds) can remove a process's fresh process lock file between its creation and its lock. The process then holds a lock on a file no
// reader finds: makeProcessLock sees the name gone and answers nothing, and takeProcessLock makes the lock again under a new token, three
// times at most. processLockOpened is the seam between the file's creation and its lock.

// processLockFiles are the names of the process lock files in the run's directory.
func processLockFiles(t *testing.T, state, run string) []string {
	t.Helper()
	entries, err := os.ReadDir(RunDir(state, run))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "process.") && strings.HasSuffix(e.Name(), ".lock") {
			out = append(out, e.Name())
		}
	}
	return out
}

// sweepBeforeTheLock makes the seam sweep the run's process lock files right after a process lock file is made, for the first n files
// (every file when n is negative), and records the paths made.
func sweepBeforeTheLock(t *testing.T, state, run string, n int) *[]string {
	t.Helper()
	var opened []string
	processLockOpened = func(path string) {
		opened = append(opened, path)
		if n < 0 || len(opened) <= n {
			sweepProcessLocks(state, run, "")
		}
	}
	t.Cleanup(func() { processLockOpened = nil })
	return &opened
}

func TestAProcessLockASweepRemovedBeforeItWasHeldIsMadeAgainUnderANewToken(t *testing.T) {
	state, run := t.TempDir(), "run-0123456789f5"
	opened := sweepBeforeTheLock(t, state, run, 1)
	p, err := takeProcessLock(state, run)
	if err != nil || p == nil {
		t.Fatalf("takeProcessLock answered %v (%v)", p, err)
	}
	defer p.Release()
	if len(*opened) != 2 || (*opened)[0] == (*opened)[1] || p.path != (*opened)[1] {
		t.Fatalf("the files made are %v and the lock is %s, want the swept one and a second under another token", *opened, p.path)
	}
	if _, err := os.Stat((*opened)[0]); !os.IsNotExist(err) {
		t.Fatalf("the swept file %s is there (%v)", (*opened)[0], err)
	}
	if !processHeld(state, run, p.Token()) {
		t.Fatalf("the process lock %s is not held where a reader finds it", p.path)
	}
	if strings.Contains((*opened)[0], p.Token()) {
		t.Fatalf("the lock kept the token of the swept file %s", (*opened)[0])
	}
	if files := processLockFiles(t, state, run); len(files) != 1 || !strings.Contains(files[0], p.Token()) {
		t.Fatalf("the run's process lock files are %v, want the one this process holds", files)
	}
	// the holder's own sweep leaves its file
	sweepProcessLocks(state, run, p.Token())
	if !processHeld(state, run, p.Token()) {
		t.Fatal("a sweep that keeps the holder's token removed its file")
	}
	p.Release()
	if files := processLockFiles(t, state, run); len(files) != 0 {
		t.Fatalf("after the release the run's process lock files are %v", files)
	}
}

func TestAProcessLockASweepRemovesThreeTimesIsAnError(t *testing.T) {
	state, run := t.TempDir(), "run-0123456789f6"
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		t.Fatal(err)
	}
	opened := sweepBeforeTheLock(t, state, run, -1)
	// one make: the file is removed before its lock, so it answers no lock and no error
	p, err := makeProcessLock(state, run)
	if p != nil || err != nil || len(*opened) != 1 {
		t.Fatalf("makeProcessLock answered %v (%v) after %d files, want nothing", p, err, len(*opened))
	}
	*opened = nil
	p, err = takeProcessLock(state, run)
	if p != nil || err == nil || !strings.Contains(err.Error(), "removed three times before it was held") {
		t.Fatalf("takeProcessLock answered %v (%v), want the error of three removals", p, err)
	}
	if len(*opened) != 3 {
		t.Fatalf("takeProcessLock made %d files, want three", len(*opened))
	}
	if files := processLockFiles(t, state, run); len(files) != 0 {
		t.Fatalf("the run's process lock files are %v, want none", files)
	}
}
