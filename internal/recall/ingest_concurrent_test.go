package recall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ingestBarrier parks each caller at the file lock seam until its partner for the same
// path arrives, so both decisions are taken before either write commits.
type ingestBarrier struct {
	mu      sync.Mutex
	waiting map[string]chan struct{}
}

func newIngestBarrier() *ingestBarrier { return &ingestBarrier{waiting: map[string]chan struct{}{}} }
func (b *ingestBarrier) hook(path string) {
	b.mu.Lock()
	ch, ok := b.waiting[path]
	if ok {
		delete(b.waiting, path)
		b.mu.Unlock()
		close(ch)
		return
	}
	ch = make(chan struct{})
	b.waiting[path] = ch
	b.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
	}
}

func ingestSetHook(t *testing.T, hook func(string)) {
	t.Helper()
	ingestBeforeFileLock = hook
	t.Cleanup(func() { ingestBeforeFileLock = nil })
}

func ingestLines(t *testing.T, prefix string, from, to int) string {
	t.Helper()
	var b strings.Builder
	for i := from; i < to; i++ {
		b.WriteString(ingestMessage(t, fmt.Sprintf("%s message %d", prefix, i)))
	}
	return b.String()
}

func ingestSecondDB(t *testing.T, path string) *RwDb {
	t.Helper()
	db, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ingestAssertConsistent checks the stored invariants: one row per (path, ord), the cursor
// equals the committed row count, and both FTS lanes index exactly the message rows.
func ingestAssertConsistent(t *testing.T, db *RwDb) {
	t.Helper()
	if dup := indexRows(t, db, "SELECT path, ord, COUNT(*) AS n FROM msgs GROUP BY path, ord HAVING n > 1"); len(dup) != 0 {
		t.Fatalf("%d (path, ord) pairs hold more than one row, first %v", len(dup), dup[0])
	}
	for _, row := range indexRows(t, db, "SELECT f.path AS path, f.last_ord AS last_ord, (SELECT COUNT(*) FROM msgs m WHERE m.path = f.path) AS n, (SELECT MAX(ord) + 1 FROM msgs m WHERE m.path = f.path) AS top FROM files f") {
		n, top := hitCountNumber(row["n"]), hitCountNumber(row["top"])
		if hitCountNumber(row["last_ord"]) != n || top != n {
			t.Fatalf("cursor of %v is %v, committed rows %v, highest ord+1 %v", row["path"], row["last_ord"], n, top)
		}
	}
	msgs := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM msgs")[0]["n"])
	for _, lane := range []string{"msgs_fts", "msgs_tri"} {
		if got := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM "+lane+"_docsize")[0]["n"]); got != msgs {
			t.Fatalf("%s indexes %v documents, msgs holds %v", lane, got, msgs)
		}
	}
}

func ingestRunTwo(t *testing.T, home string, a, b *RwDb) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, db := range []*RwDb{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = ingest(home, db, 0)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// CRW-1154 (A8-01): two refreshes that read the same cursor before either commits.
func TestIngestConcurrentAppendKeepsOneRowPerOrdinal(t *testing.T) {
	db, path := indexTestDB(t)
	other := ingestSecondDB(t, path)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestLines(t, "first", 0, 10))
	ingestForTest(t, home, db, 0)
	before, _ := os.ReadFile(rollout)
	ingestWrite(t, rollout, append(before, []byte(ingestLines(t, "grown", 0, 2000))...))
	ingestSetHook(t, newIngestBarrier().hook)
	ingestRunTwo(t, home, db, other)
	ingestAssertConsistent(t, db)
	if n := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM msgs")[0]["n"]); n != 2010 {
		t.Fatalf("%v messages, want 2010", n)
	}
}

// The appended lines arrive after both refreshes have decided, before either holds the lock.
func TestIngestAppendLandingAfterTheDecisionIsStillOneRowPerOrdinal(t *testing.T) {
	db, path := indexTestDB(t)
	other := ingestSecondDB(t, path)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestLines(t, "first", 0, 10))
	ingestForTest(t, home, db, 0)
	before, _ := os.ReadFile(rollout)
	ingestWrite(t, rollout, append(before, []byte(ingestLines(t, "grown", 0, 5))...))
	barrier := newIngestBarrier()
	var once sync.Once
	ingestSetHook(t, func(p string) {
		barrier.hook(p)
		once.Do(func() {
			now, _ := os.ReadFile(rollout)
			ingestWrite(t, rollout, append(now, []byte(ingestLines(t, "late", 0, 2000))...))
		})
	})
	ingestRunTwo(t, home, db, other)
	ingestAssertConsistent(t, db)
	// A later refresh finishes whatever the two did not see.
	ingestForTest(t, home, db, 0)
	ingestAssertConsistent(t, db)
	if n := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM msgs")[0]["n"]); n != 2015 {
		t.Fatalf("%v messages, want 2015", n)
	}
}

func TestIngestConcurrentFirstIngest(t *testing.T) {
	db, path := indexTestDB(t)
	other := ingestSecondDB(t, path)
	home := t.TempDir()
	for i := range 3 {
		writeRolloutTestFile(t, home, fmt.Sprintf("sessions/2026/01/0%d/f.jsonl", i+1), ingestLines(t, fmt.Sprint("file", i), 0, 40))
	}
	ingestSetHook(t, newIngestBarrier().hook)
	ingestRunTwo(t, home, db, other)
	ingestAssertConsistent(t, db)
	if n := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM msgs")[0]["n"]); n != 120 {
		t.Fatalf("%v messages, want 120", n)
	}
}

// A refresh that chose to append finds, once it holds the lock, that the file was pruned
// and has come back longer: it must index the whole file, not graft the tail on nothing.
func TestIngestAppendAfterPruneRebuildsFromTheFileStart(t *testing.T) {
	db, path := indexTestDB(t)
	other := ingestSecondDB(t, path)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestLines(t, "first", 0, 10))
	ingestForTest(t, home, db, 0)
	before, _ := os.ReadFile(rollout)
	ingestWrite(t, rollout, append(before, []byte(ingestLines(t, "grown", 0, 5))...))
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ingestSetHook(t, func(string) {
		once.Do(func() { close(parked); <-release })
	})
	done := make(chan error, 1)
	go func() { _, err := ingest(home, db, 0); done <- err }()
	<-parked
	aside := filepath.Join(t.TempDir(), "a.jsonl")
	if err := os.Rename(rollout, aside); err != nil {
		t.Fatal(err)
	}
	ingestSetHook(t, nil)
	if r, err := ingest(home, other, 0); err != nil || r.Pruned != 1 {
		t.Fatal(r, err)
	}
	if err := os.Rename(aside, rollout); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ingestAssertConsistent(t, db)
	if n := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM msgs")[0]["n"]); n != 15 {
		t.Fatalf("%v messages, want 15", n)
	}
}

// The file vanishes between the decision and the lock: a skipped file, not a failed refresh.
func TestIngestFileRemovedBeforeItsLockIsSkipped(t *testing.T) {
	db, _ := indexTestDB(t)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestLines(t, "first", 0, 10))
	ingestSetHook(t, func(string) { _ = os.Remove(rollout) })
	if _, err := ingest(home, db, 0); err != nil {
		t.Fatal(err)
	}
	ingestAssertConsistent(t, db)
}

// A database written before the invariant existed holds duplicate rows; opening it repairs it.
func TestIndexOpenRepairsDuplicateRows(t *testing.T) {
	db, path := indexTestDB(t)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestLines(t, "first", 0, 20))
	ingestForTest(t, home, db, 0)
	recallSQL(t, db, "UPDATE meta SET value='2' WHERE key='schema_version'; DROP INDEX IF EXISTS idx_msgs_path_ord;")
	recallSQL(t, db, "INSERT INTO msgs (path, ord, ts, role, match_field, synthetic, text) SELECT path, ord, ts, role, match_field, synthetic, text FROM msgs")
	recallSQL(t, db, "INSERT INTO recall_hit_counts (ref, hit_count, last_hit_at) VALUES ('thread:keep', 4, 'stamp')")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	// The derived rows are dropped; the next refresh rebuilds them from the rollouts.
	if r := ingestForTest(t, home, repaired, 0); r.Ingested != 1 || r.Msgs != 20 {
		t.Fatal(r)
	}
	ingestAssertConsistent(t, repaired)
	if got := readHitCounts(repaired, []string{"thread:keep"}); got["thread:keep"] != 4 {
		t.Fatal("hit history was not preserved", got)
	}
	if _, err := os.Stat(rollout); err != nil {
		t.Fatal("the rollout is the source and must stay", err)
	}
	if err := repaired.Exec("INSERT INTO msgs (path, ord, ts, role, match_field, synthetic, text) VALUES ('x', 0, '', '', '', 0, ''), ('x', 0, '', '', '', 0, '')"); err == nil {
		t.Fatal("(path, ord) is not unique")
	}
}
