package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CRW-1189 (R3-S2-F2): a session lock that stayed held for the whole wait is a retryable state, and its text says so. The
// error is still the busy error of the lock (errors.Is fs.ErrExist, the *fs.PathError of the lock file) and the wait
// (the oracle's schedule) is unchanged; only the words differ from the bare "open <lock>: file exists".
func TestCRW1189BusySessionLockSaysItIsRetryable(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, lockOwnerLive()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(lockPath) }()
	slept := time.Duration(0)
	ran := false
	err := withSessionLock(cwd, "s", func() error { ran = true; return nil }, func(d time.Duration) { slept += d })
	if err == nil || ran {
		t.Fatalf("err=%v ran=%v; want the busy error and fn not run", err, ran)
	}
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrExist) || !errors.As(err, &pathErr) || pathErr.Path != lockPath {
		t.Errorf("the busy error stays the lock file's fs.ErrExist: %v", err)
	}
	for _, want := range []string{"session state is busy", "retry", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the text must say %q: %q", want, err.Error())
		}
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Errorf("the lock file stays named for diagnosis: %q", err.Error())
	}
	if slept != 250*time.Millisecond {
		t.Errorf("the oracle's wait is unchanged: slept %v", slept)
	}
}

// A failure that is not contention keeps its own text.
func TestCRW1189OtherLockFailuresKeepTheirText(t *testing.T) {
	if err := WithSessionLock(t.TempDir(), "a/b", func() error { return nil }); !errors.Is(err, ErrNonCanonicalSessionID) || strings.Contains(err.Error(), "busy") {
		t.Errorf("non-canonical id: %v", err)
	}
}

// CRW-1189 round 2 (d2): only contention is retryable. A lock the acquisition rules cannot judge (a record that is empty or not of
// either protocol, a symlink, a directory) can stay as it is with no command running, so its text does not send the caller into
// retries: it names the path, says the owner cannot be judged and what to inspect. It is still the lock's fs.ErrExist.
func TestCRW1189UnjudgeableSessionLockDoesNotAskForARetry(t *testing.T) {
	for name, seed := range map[string]func(t *testing.T, cwd string) string{
		"an unknown record": func(t *testing.T, cwd string) string { return lockOwnerSeed(t, cwd, "held") },
		"an empty record":   func(t *testing.T, cwd string) string { return lockOwnerSeed(t, cwd, "") },
		"a symlink": func(t *testing.T, cwd string) string {
			if err := makeSessionsDir(cwd); err != nil {
				t.Fatal(err)
			}
			lock := SessionLockPath(cwd, "s")
			if err := os.Symlink(filepath.Join(cwd, "nowhere"), lock); err != nil {
				t.Fatal(err)
			}
			return lock
		},
		"a directory": func(t *testing.T, cwd string) string {
			if err := makeSessionsDir(cwd); err != nil {
				t.Fatal(err)
			}
			lock := SessionLockPath(cwd, "s")
			if err := os.Mkdir(lock, 0o755); err != nil {
				t.Fatal(err)
			}
			return lock
		},
	} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			lock := seed(t, cwd)
			err := withSessionLock(cwd, "s", func() error { t.Fatal("fn ran"); return nil }, func(time.Duration) {})
			if err == nil || !errors.Is(err, fs.ErrExist) {
				t.Fatalf("err=%v; want the lock's fs.ErrExist", err)
			}
			var pathErr *fs.PathError
			if !errors.As(err, &pathErr) || pathErr.Path != lock {
				t.Errorf("the lock file's *fs.PathError is not reachable: %v", err)
			}
			text := err.Error()
			for _, bad := range []string{"retry", "another crw command is using"} {
				if strings.Contains(text, bad) {
					t.Errorf("an unjudgeable lock must not claim contention (%q): %q", bad, text)
				}
			}
			for _, want := range []string{"cannot be judged", lock, "nothing was changed"} {
				if !strings.Contains(text, want) {
					t.Errorf("the text lacks %q: %q", want, text)
				}
			}
		})
	}
}

// Known contention (a live holder in this process, a live pid in the oracle's record) keeps the retry text.
func TestCRW1189LiveHolderKeepsTheRetryText(t *testing.T) {
	cwd := t.TempDir()
	lockOwnerSeed(t, cwd, strconv.Itoa(os.Getpid()))
	err := withSessionLock(cwd, "s", func() error { t.Fatal("fn ran"); return nil }, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "another crw command is using this session; retry") {
		t.Errorf("a live oracle holder is contention: %v", err)
	}
}
