package state

import (
	"errors"
	"io/fs"
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
