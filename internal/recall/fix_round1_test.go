package recall

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-1123 :499 -- what cannot be read of a directory is not evidence that its files are gone.
func TestUnreadableDirectoryIsReportedAndNotPruned(t *testing.T) {
	db, _ := indexTestDB(t)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestMessage(t, "retainedterm opening"))
	other := writeRolloutTestFile(t, home, "sessions/2026/01/02/b.jsonl", ingestMessage(t, "otherterm opening"))
	ingestForTest(t, home, db, 0)
	dir := filepath.Dir(rollout)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("the user can read a mode-zero directory")
	}
	body, _ := os.ReadFile(other)
	ingestWrite(t, other, append(body, []byte(ingestMessage(t, "othernew tail"))...))
	result, err := ingest(home, db, 0)
	if err != nil || result.Pruned != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if rows := indexRows(t, db, "SELECT text FROM msgs WHERE path = '"+rollout+"'"); len(rows) != 1 {
		t.Errorf("the unreadable directory's rows were pruned: %v", rows)
	}
	if got := continuityHas(t, db, "othernew"); got != 1 {
		t.Errorf("the readable rest of the home was not collected: %d", got)
	}
	fresh, err := measureIndexFreshness(home, db, 0, nil)
	if err != nil || fresh.ExtraFiles != 0 {
		t.Errorf("%+v %v", fresh, err)
	}
	// Once readable again and really removed, the rows go.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rollout); err != nil {
		t.Fatal(err)
	}
	if result, err := ingest(home, db, 0); err != nil || result.Pruned != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}

// CRW-1154 -- the prune decision is taken under the lock: a file that has come back and been appended
// since the listing keeps its newer commit.
func TestIngestPruneCannotEraseAFileThatCameBack(t *testing.T) {
	db, path := indexTestDB(t)
	other := ingestSecondDB(t, path)
	home := t.TempDir()
	a := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestMessage(t, "oldterm opening"))
	b := writeRolloutTestFile(t, home, "sessions/2026/01/02/b.jsonl", ingestMessage(t, "other opening"))
	ingestForTest(t, home, db, 0)
	old, _ := os.ReadFile(a)
	aside := filepath.Join(t.TempDir(), "a.jsonl")
	if err := os.Rename(a, aside); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(b)
	ingestWrite(t, b, append(body, []byte(ingestMessage(t, "other tail"))...))
	parked, release := make(chan struct{}), make(chan struct{})
	ingestSetHook(t, func(p string) {
		if p == b {
			close(parked)
			<-release
		}
	})
	done := make(chan error, 1)
	go func() { _, err := ingest(home, db, 0); done <- err }()
	<-parked
	ingestBeforeFileLock = nil
	if err := os.Rename(aside, a); err != nil {
		t.Fatal(err)
	}
	ingestWrite(t, a, append(old, []byte(ingestMessage(t, "newterm tail"))...))
	if _, err := ingest(home, other, 0); err != nil {
		t.Fatal(err)
	}
	if got := continuityHas(t, other, "newterm"); got != 1 {
		t.Fatal("setup", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := continuityHas(t, db, "newterm"); got != 1 {
		t.Errorf("a stale prune deleted the newer commit of an existing file: %d", got)
	}
	ingestAssertConsistent(t, db)
}
