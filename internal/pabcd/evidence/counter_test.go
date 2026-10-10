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
		{"short", "a1", ""}, {"short-x", "a1", ""}, {"short", "", ""}, {"short", "", "t1"}, {"short", "x-a1", ""}, {"short", "x", "a1"},
		{"019f0000-0000-7000-8000-000000000001", "019f0000-0000-7000-8000-000000000002", "019f0000-0000-7000-8000-000000000003"},
	} {
		name := filepath.Base(attemptsPath(cwd, c.session, c.agent, c.turn))
		if got := counterOwner(name); got != c.session {
			t.Errorf("%s: owner %q, want %q", name, got, c.session)
		}
	}
	for _, name := range []string{"s1-x-a1-1.json", "short-a-" + tupleDigest("a/", "") + ".json", "x.json"} {
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
	if !HasSpentBudget(cwd, "a-b") || HasSpentBudget(cwd, "a/b") {
		t.Fatal("a-b's spent counter must hold a-b, and a/b's own record of the tuple supersedes the shared name for a/b")
	}
	put(t, attemptsPath(cwd, "a-b", "y", "t"), []byte("{\"attempts\":3}\n")) // a name no record of a/b explains: may be a/b's own
	if !HasSpentBudget(cwd, "a/b") {
		t.Fatal("a spent counter of the oracle's layout stopped counting")
	}
	_ = os.Remove(attemptsPath(cwd, "a-b", "y", "t"))
	ClearAttempts(cwd, "a-b", "x", "t")
	if HasSpentBudget(cwd, "a/b") || ReadCounter(cwd, "a/b", "x", "t") != (Counter{CounterActive, 1}) {
		t.Fatal("clearing a-b changed a/b")
	}
	put(t, counterPath(cwd, "a/b", "x", "t"), []byte(`{"attempts":1,"sessionId":"a-b","agentId":"x","turnId":"t"}`))
	if got := ReadCounter(cwd, "a/b", "x", "t"); got.State != CounterCorrupt || !HasSpentBudget(cwd, "a/b") {
		t.Fatalf("a record naming another session read as %+v", got)
	}
}

// CRW-1106 verification round 1: a counter written now for an agent or a turn that sanitising changes still names its session
// exactly, so a session whose key is a prefix of it (short for short-x) is not held by it; a counter without that identity (an old
// file) keeps denying every session it may belong to.
func TestFreshCounterOwnershipWithRawActor(t *testing.T) {
	for _, c := range []struct{ agent, turn string }{{"a/b", "t"}, {"a1", "t/1"}, {"a b", ""}, {"a/b", "t/1"}} {
		cwd := t.TempDir()
		if !WriteAttempts(cwd, "short-x", c.agent, MaxAttempts, c.turn) {
			t.Fatal("not written")
		}
		if !HasSpentBudget(cwd, "short-x") {
			t.Errorf("%+v: the session's own spent counter is not seen", c)
		}
		if HasSpentBudget(cwd, "short") {
			t.Errorf("%+v: a counter of short-x holds short", c)
		}
		if got := ReadCounter(cwd, "short-x", c.agent, c.turn); got != (Counter{CounterExhausted, MaxAttempts}) {
			t.Errorf("%+v: read %+v", c, got)
		}
		ClearAttempts(cwd, "short-x", c.agent, c.turn)
		if HasSpentBudget(cwd, "short-x") {
			t.Errorf("%+v: cleared counter still spent", c)
		}
	}
	// Old files without identity: unexplained, so every session that may own them is held.
	cwd := t.TempDir()
	put(t, attemptsPath(cwd, "short-x", "a/b", "t"), []byte("{\"attempts\":3}\n"))
	if !HasSpentBudget(cwd, "short") || !HasSpentBudget(cwd, "short-x") {
		t.Fatal("an old counter whose owner cannot be told stopped denying")
	}
	// The old file is still this tuple's counter until the tuple writes its own; writing moves it, and a receipt clears both.
	if got := ReadCounter(cwd, "short-x", "a/b", "t"); got != (Counter{CounterExhausted, MaxAttempts}) {
		t.Fatalf("the old counter of the tuple read as %+v", got)
	}
	if !WriteAttempts(cwd, "short-x", "a/b", 1, "t") {
		t.Fatal("not written")
	}
	if HasSpentBudget(cwd, "short") || HasSpentBudget(cwd, "short-x") || ReadCounter(cwd, "short-x", "a/b", "t") != (Counter{CounterActive, 1}) {
		t.Fatal("the written counter did not replace the old file")
	}
	put(t, attemptsPath(cwd, "short-x", "a/b", "t"), []byte("{\"attempts\":3}\n"))
	ClearAttempts(cwd, "short-x", "a/b", "t")
	if HasSpentBudget(cwd, "short-x") || ReadCounter(cwd, "short-x", "a/b", "t").State != CounterMissing {
		t.Fatal("clearing left a counter of the tuple")
	}
	// A record naming another session is not this tuple's counter.
	put(t, counterPath(cwd, "short-x", "a/c", "t"), []byte("{\"attempts\":3,\"sessionId\":\"short\",\"agentId\":\"a/c\",\"turnId\":\"t\"}\n"))
	if got := ReadCounter(cwd, "short-x", "a/c", "t"); got.State != CounterCorrupt {
		t.Fatalf("a record of another session read as %+v", got)
	}
}

// CRW-1106 post-evaluation round (d1): the flat counter of the oracle's layout under a non-canonical session's sanitised name is that
// session's counter until a receipt clears it, but it may be a canonical twin's, so clearing never deletes it: a/b's own record of the
// tuple supersedes it for a/b, and the twin keeps its budget.
func TestLegacyCounterOfANonCanonicalSession(t *testing.T) {
	cwd := t.TempDir()
	flat := attemptsPath(cwd, "a/b", "x", "t")
	put(t, flat, []byte("{\"attempts\":3}\n"))
	if got := ReadCounter(cwd, "a/b", "x", "t"); got != (Counter{CounterExhausted, MaxAttempts}) {
		t.Fatalf("the flat counter read as %+v for a/b", got)
	}
	if !HasSpentBudget(cwd, "a/b") || !HasSpentBudget(cwd, "a-b") {
		t.Fatal("an exhausted flat counter stopped counting")
	}
	ClearAttempts(cwd, "a/b", "x", "t")
	if got := ReadCounter(cwd, "a/b", "x", "t"); got.Spent() {
		t.Fatalf("a/b's cleared tuple still reads as %+v", got)
	}
	if HasSpentBudget(cwd, "a/b") {
		t.Fatal("a/b is still held by the counter it cleared")
	}
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("the flat counter, which a-b may own, was deleted: %v", err)
	}
	if got := ReadCounter(cwd, "a-b", "x", "t"); got != (Counter{CounterExhausted, MaxAttempts}) || !HasSpentBudget(cwd, "a-b") {
		t.Fatalf("a/b's clear changed the twin a-b: %+v", got)
	}
	// An active flat counter carries its count into a/b's next attempt, and a/b's own write supersedes it.
	other := t.TempDir()
	put(t, attemptsPath(other, "a/b", "x", "t"), []byte("{\"attempts\":2}\n"))
	if got := ReadCounter(other, "a/b", "x", "t"); got != (Counter{CounterActive, 2}) {
		t.Fatalf("the flat counter read as %+v", got)
	}
	if !WriteAttempts(other, "a/b", "x", 3, "t") || ReadCounter(other, "a/b", "x", "t") != (Counter{CounterExhausted, MaxAttempts}) {
		t.Fatal("a/b's own write did not supersede the flat counter")
	}
	ClearAttempts(other, "a/b", "x", "t")
	if HasSpentBudget(other, "a/b") || ReadCounter(other, "a/b", "x", "t").Spent() {
		t.Fatal("a/b is held after its receipt")
	}
}
