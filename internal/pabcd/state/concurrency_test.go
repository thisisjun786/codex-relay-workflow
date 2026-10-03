package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// TestHelperProcess is the child of the process tests below, not a test of its own: the oracle runs its concurrency cases in
// child processes too, and a lock, a rename and a kill are only real between processes.
func TestHelperProcess(t *testing.T) {
	mode, cwd, arg := os.Getenv("CRW_STATE_HELPER"), os.Getenv("CRW_STATE_CWD"), os.Getenv("CRW_STATE_ARG")
	if mode == "" {
		t.Skip("child of the process tests")
	}
	log := filepath.Join(cwd, "log")
	note := func(line string) {
		f, _ := os.OpenFile(log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
	waitFor := func(text string) { // a wait nobody answers ends the child
		for i := 0; i < 1500; i++ {
			if b, _ := os.ReadFile(log); strings.Contains(string(b), text) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		os.Exit(2)
	}
	s, err := DefaultState("conc", "slug-"+arg), error(nil)
	s.Phase = Phase(arg)
	switch mode {
	case "write":
		err = WriteState(cwd, s)
	case "ensure":
		var created bool
		created, err = EnsureState(cwd, "conc")
		fmt.Print(created)
	case "kill": // dies inside the rename step, between the temp write and the publication
		err = writeState(cwd, s, at(), func(string, string) error { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL); select {} })
	case "hold": // owns the lock until the waiter has met it, and says when it has let go
		err = WithSessionLock(cwd, "conc", func() error {
			note("A-in")
			waitFor("B-got-EEXIST")
			note("A-out")
			return nil
		})
		note("A-released")
	case "wait": // tries only after the holder owns the lock; the sleep seam runs after a create that failed and waits for the release, not for a time
		waitFor("A-in")
		err = withSessionLock(cwd, "conc", func() error { note("B-in"); return nil }, func(time.Duration) { note("B-got-EEXIST"); waitFor("A-released") })
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// children starts a helper process per (mode, arg) pair together and returns what each printed.
func children(t *testing.T, cwd string, pairs ...[2]string) (outs []string) {
	t.Helper()
	outs, errs := make([]string, len(pairs)), make([]error, len(pairs))
	var wg sync.WaitGroup
	for i, p := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
			cmd.Env = append(os.Environ(), "CRW_STATE_HELPER="+p[0], "CRW_STATE_CWD="+cwd, "CRW_STATE_ARG="+p[1])
			b, err := cmd.Output()
			outs[i], errs[i] = string(b), err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		var killed *exec.ExitError
		if err != nil && !(errors.As(err, &killed) && pairs[i][0] == "kill") {
			t.Fatalf("%s %s: %v %s", pairs[i][0], pairs[i][1], err, killed)
		}
	}
	return outs
}

func tempFiles(cwd string) (tmps []string) {
	for _, n := range sessionFiles(cwd) {
		if strings.HasSuffix(n, ".tmp") {
			tmps = append(tmps, n)
		}
	}
	return tmps
}

func TestConcurrentWritersLeaveOneCompleteDocument(t *testing.T) { // recorded concurrent_writers
	for round := 0; round < 10; round++ {
		cwd := t.TempDir()
		children(t, cwd, [2]string{"write", "P"}, [2]string{"write", "A"}, [2]string{"write", "B"}, [2]string{"write", "C"})
		got, unreadable := ReadStateStrict(cwd, "conc")
		if unreadable || got.Slug != "slug-"+string(got.Phase) || len(tempFiles(cwd)) != 0 {
			t.Fatalf("round %d: %+v unreadable=%v tmp=%v", round, got, unreadable, tempFiles(cwd))
		}
	}
}

func TestConcurrentCreatorsCreateTheSessionOnceAndPublishOneIgnoreFile(t *testing.T) { // oracle 87, recorded concurrent_ensure_state
	for round := 0; round < 3; round++ {
		cwd := t.TempDir()
		outs := children(t, cwd, [2]string{"ensure", ""}, [2]string{"ensure", ""}, [2]string{"ensure", ""}, [2]string{"ensure", ""})
		got, unreadable := ReadStateStrict(cwd, "conc")
		if n := strings.Count(strings.Join(outs, ""), "true"); n != 1 || unreadable || got.SessionID != "conc" || fileText(t, filepath.Join(cwd, crwdir.DirName, ".gitignore")) != crwdir.GitignoreText || len(tempFiles(cwd)) != 0 {
			t.Fatalf("round %d: created %d times %q, %+v unreadable=%v", round, n, outs, got, unreadable)
		}
	}
}

func TestWritersInOneProcessNeverShareATempFile(t *testing.T) { // the hazard Node cannot have: its writeState runs one at a time
	cwd, errs := t.TempDir(), make(chan error, 64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				s := DefaultState("conc", fmt.Sprintf("slug-%d-%d", i, j))
				errs <- WriteState(cwd, s)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, unreadable := ReadStateStrict(cwd, "conc"); unreadable || len(tempFiles(cwd)) != 0 {
		t.Fatalf("unreadable=%v tmp=%v", unreadable, tempFiles(cwd))
	}
}

func TestKilledBetweenTempWriteAndRenameKeepsThePreviousStateAndLeavesAnOrphan(t *testing.T) { // recorded killed_between_temp_write_and_rename
	cwd, previous := t.TempDir(), DefaultState("conc", "")
	previous.Phase = PhaseB
	if err := WriteState(cwd, previous); err != nil {
		t.Fatal(err)
	}
	children(t, cwd, [2]string{"kill", "P"})
	orphans := tempFiles(cwd)
	if len(orphans) != 1 || ReadState(cwd, "conc").Phase != PhaseB {
		t.Fatalf("orphans %v, phase %s", orphans, ReadState(cwd, "conc").Phase)
	}
	if s, unreadable := restore("conc", []byte(fileText(t, filepath.Join(cwd, crwdir.DirName, SessionsSubdir, orphans[0]))), at()); unreadable || s.Phase != PhaseP {
		t.Fatalf("the orphan holds %+v", s) // the temp file was complete when the process died
	}
	previous.Phase = PhaseC // nothing sweeps the orphan, and the next write goes through
	if err := WriteState(cwd, previous); err != nil || ReadState(cwd, "conc").Phase != PhaseC || len(tempFiles(cwd)) != 1 {
		t.Fatalf("%v phase %s orphans %v", err, ReadState(cwd, "conc").Phase, tempFiles(cwd))
	}
}

func TestFailedRenameReturnsTheErrorRemovesTheTempAndKeepsThePreviousState(t *testing.T) {
	cwd, s, boom := t.TempDir(), DefaultState("conc", ""), errors.New("rename failed")
	s.Phase = PhaseB
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	s.Phase = PhaseP
	if err := writeState(cwd, s, at(), func(string, string) error { return boom }); err != boom || ReadState(cwd, "conc").Phase != PhaseB || len(tempFiles(cwd)) != 0 {
		t.Fatalf("%v phase %s tmp %v", err, ReadState(cwd, "conc").Phase, tempFiles(cwd))
	}
}

func TestHeldLockExhaustsTheScheduleAndIsNeverBroken(t *testing.T) { // recorded lock_held_exhausts
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lock := StatePath(cwd, "s") + ".lock"
	old := time.Now().Add(-240 * time.Hour)
	if err := os.WriteFile(lock, []byte("424242"), 0o644); err != nil || os.Chtimes(lock, old, old) != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	entered := false
	err := withSessionLock(cwd, "s", func() error { entered = true; return nil }, func(d time.Duration) { slept = append(slept, d) })
	var pathErr *fs.PathError
	want := []time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35}
	for i := range want {
		want[i] *= time.Millisecond
	}
	if !errors.As(err, &pathErr) || pathErr.Path != lock || !errors.Is(err, fs.ErrExist) || entered || !slices.Equal(slept, want) || fileText(t, lock) != "424242" {
		t.Fatalf("err %v entered %v slept %v", err, entered, slept)
	}
}

func TestLockIsHeldWithThePidAndReleasedAfterReturnErrorAndPanic(t *testing.T) { // recorded lock_runs_and_releases, lock_released_when_fn_throws
	cwd, lock, boom := t.TempDir(), "", errors.New("boom")
	for name, fn := range map[string]func() error{"return": func() error { return nil }, "error": func() error { return boom }, "panic": func() error { panic(boom) }} {
		lock = StatePath(cwd, "s") + ".lock"
		var recovered any
		err := func() (err error) {
			defer func() { recovered = recover() }()
			return WithSessionLock(cwd, "s", func() error {
				if got := fileText(t, lock); got != fmt.Sprint(os.Getpid()) {
					t.Errorf("%s: lock holds %q", name, got)
				}
				return fn()
			})
		}()
		if _, statErr := os.Stat(lock); !errors.Is(statErr, fs.ErrNotExist) || (name == "error") != (err == boom) || (name == "panic") != (recovered == boom) {
			t.Errorf("%s: lock still there, or wrong error %v or panic %v (%v)", name, err, recovered, statErr)
		}
	}
}

func TestSecondProcessWaitsForTheHolderAndEntersAfterIt(t *testing.T) { // recorded lock_contention_between_processes
	cwd := t.TempDir()
	children(t, cwd, [2]string{"hold", ""}, [2]string{"wait", ""})
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(fileText(t, filepath.Join(cwd, "log"))), "\n") {
		if line != "B-got-EEXIST" {
			order = append(order, line)
		}
	}
	if !slices.Equal(order, []string{"A-in", "A-out", "A-released", "B-in"}) || !strings.Contains(fileText(t, filepath.Join(cwd, "log")), "B-got-EEXIST") {
		t.Fatalf("%q", fileText(t, filepath.Join(cwd, "log")))
	}
	if _, err := os.Stat(StatePath(cwd, "conc") + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("lock left behind")
	}
}

func TestLockedReadModifyWriteLosesNoUpdate(t *testing.T) {
	cwd, errs := t.TempDir(), make(chan error, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WithSessionLock(cwd, "counter", func() error {
				s := ReadState(cwd, "counter")
				s.IdleEditNudges++
				return WriteState(cwd, s)
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := ReadState(cwd, "counter").IdleEditNudges; got != 6 {
		t.Fatalf("%v updates survived of 6", got)
	}
}
