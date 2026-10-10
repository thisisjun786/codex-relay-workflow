package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1108 (known-defects.md:172): the evidence writers and the session reads take a canonical session id only. The oracle names
// the counter and marker files by SanitizeKey of the id, so a/b wrote, read and cleared a-b's counter, and the empty id the
// counter of "missing". A non-canonical id now creates nothing and touches no file of the canonical id it would have aliased: a
// write reports failure, a clear does nothing, and a read answers on the closed side (the budget spent, the marker status
// unreadable), so an id that cannot name its own files never reads as a session with a clean record.
func TestEvidenceBoundariesRefuseNonCanonicalSessionIDs(t *testing.T) {
	for _, c := range []struct{ alias, canonical string }{{"a/b", "a-b"}, {"", "missing"}, {"s 1", "s-1"}} {
		cwd := t.TempDir()
		if !WriteAttempts(cwd, c.canonical, "x", 2, "t") {
			t.Fatalf("%q: the canonical counter could not be written", c.canonical)
		}
		before := attemptsDir(cwd)
		if WriteAttempts(cwd, c.alias, "x", 1, "t") {
			t.Errorf("WriteAttempts(%q) reported a write", c.alias)
		}
		if got := ReadAttempts(cwd, c.alias, "x", "t"); got != MaxAttempts {
			t.Errorf("ReadAttempts(%q) = %d; want MaxAttempts, never %s's counter", c.alias, got, c.canonical)
		}
		ClearAttempts(cwd, c.alias, "x", "t")
		if got := ReadAttempts(cwd, c.canonical, "x", "t"); got != 2 {
			t.Errorf("%q: the canonical counter reads %d after the alias wrote and cleared; want 2", c.canonical, got)
		}
		if after := attemptsDir(cwd); len(after) != len(before) {
			t.Errorf("%q: the counters changed from %v to %v", c.alias, before, after)
		}
		if !HasSpentBudget(cwd, c.alias) {
			t.Errorf("HasSpentBudget(%q) = false; want the closed answer", c.alias)
		}

		freshLock := t.TempDir()
		ran := false
		if err := WithCounterLock(freshLock, c.alias, "x", "t", func() error { ran = true; return nil }); !errors.Is(err, state.ErrNonCanonicalSessionID) || ran {
			t.Errorf("WithCounterLock(%q) = %v, ran %v; want refusal before the callback", c.alias, err, ran)
		}
		if _, err := os.Lstat(filepath.Join(freshLock, ".crw")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q: a refused counter lock created .crw (%v)", c.alias, err)
		}
		if got := ReadCounter(freshLock, c.alias, "x", "t"); got != (Counter{State: CounterUnreadable}) {
			t.Errorf("ReadCounter(%q) = %+v; want unreadable", c.alias, got)
		}

		fresh := t.TempDir()
		if err := WriteUnrecordableMarker(fresh, c.alias, "x"); !errors.Is(err, state.ErrNonCanonicalSessionID) {
			t.Errorf("WriteUnrecordableMarker(%q) = %v; want ErrNonCanonicalSessionID", c.alias, err)
		}
		if got := UnrecordableVerdictStatus(fresh, c.alias); got != (VerdictStatus{Unreadable: true}) {
			t.Errorf("UnrecordableVerdictStatus(%q) = %+v; want unreadable", c.alias, got)
		}
		if _, err := os.Lstat(filepath.Join(fresh, ".crw")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q: a refused id created .crw (%v)", c.alias, err)
		}
	}
	// the canonical a-b keeps its own marker; a/b neither writes nor reads it
	cwd := t.TempDir()
	must(t, WriteUnrecordableMarker(cwd, "a-b", "x"))
	if got := UnrecordableVerdictStatus(cwd, "a-b"); !got.Present {
		t.Errorf("a-b's own marker: %+v", got)
	}
}
