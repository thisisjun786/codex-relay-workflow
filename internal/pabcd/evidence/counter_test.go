package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// CRW-1106: the counter name in the oracle's layout tells its owner when a reading reproduces its digest.
func TestCounterOwnerReadings(t *testing.T) {
	cwd := t.TempDir()
	for _, c := range []struct{ session, agent, turn string }{
		{"short", "a1", ""}, {"short-x", "a1", ""}, {"short", "x-a1", ""}, {"short", "x", "a1"},
		{"019f0000-0000-7000-8000-000000000001", "019f0000-0000-7000-8000-000000000002", "019f0000-0000-7000-8000-000000000003"},
	} {
		name := filepath.Base(attemptsPath(cwd, c.session, c.agent, c.turn))
		if got := counterOwner(name); got != c.session {
			t.Errorf("%s: owner %q, want %q", name, got, c.session)
		}
	}
	for _, name := range []string{"s1-x-a1-1.json", "short-missing-" + tupleDigest("", "") + ".json", "short-a-" + tupleDigest("a/", "") + ".json", "x.json"} {
		if got := counterOwner(name); got != "" {
			t.Errorf("%s: owner %q, want none (an unexplained name counts for every session it may belong to)", name, got)
		}
	}
}

// The record lock leaves nothing behind, and two holders never run together.
func TestCounterLockLeavesNothing(t *testing.T) {
	cwd := t.TempDir()
	var mu sync.Mutex
	inside, overlap := 0, false
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			err := WithCounterLock(cwd, "s1", "a1", "t1", func() error {
				mu.Lock()
				inside++
				overlap = overlap || inside > 1
				mu.Unlock()
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if overlap {
		t.Fatal("two holders ran together")
	}
	entries, err := os.ReadDir(filepath.Join(cwd, ".crw"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("lock file left: %s", e.Name())
		}
	}
}

// A canonical session keeps the oracle's file and bytes; a session id that sanitising changes gets its own directory, so a/b and
// a-b never share a budget. A spent counter of the twin's still counts for a/b in the oracle's layout (it cannot be told whose).
func TestCounterLayoutBySessionShape(t *testing.T) {
	cwd := t.TempDir()
	if !WriteAttempts(cwd, "a-b", "x", 3, "t") || !WriteAttempts(cwd, "a/b", "x", 1, "t") {
		t.Fatal("not written")
	}
	raw, err := os.ReadFile(attemptsPath(cwd, "a-b", "x", "t"))
	if err != nil || string(raw) != "{\"attempts\":3}\n" {
		t.Fatalf("canonical file %q %v", raw, err)
	}
	if got := ReadCounter(cwd, "a/b", "x", "t"); got != (Counter{CounterActive, 1}) {
		t.Fatalf("a/b read %+v", got)
	}
	if got := ReadCounter(cwd, "a-b", "x", "t"); got != (Counter{CounterExhausted, 3}) {
		t.Fatalf("a-b read %+v", got)
	}
	if !HasSpentBudget(cwd, "a-b") || !HasSpentBudget(cwd, "a/b") {
		t.Fatal("a spent counter of the oracle's layout stopped counting")
	}
	ClearAttempts(cwd, "a-b", "x", "t")
	if HasSpentBudget(cwd, "a/b") || ReadCounter(cwd, "a/b", "x", "t") != (Counter{CounterActive, 1}) {
		t.Fatal("clearing a-b changed a/b")
	}
	put(t, counterPath(cwd, "a/b", "x", "t"), []byte(`{"attempts":1,"sessionId":"a-b","agentId":"x","turnId":"t"}`))
	if got := ReadCounter(cwd, "a/b", "x", "t"); got.State != CounterCorrupt || !HasSpentBudget(cwd, "a/b") {
		t.Fatalf("a record naming another session read as %+v", got)
	}
}
