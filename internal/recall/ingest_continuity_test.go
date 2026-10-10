package recall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func continuityHas(t *testing.T, db *RwDb, term string) int {
	t.Helper()
	fts := len(indexRows(t, db, `SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH '"`+term+`"'`))
	tri := len(indexRows(t, db, `SELECT rowid FROM msgs_tri WHERE msgs_tri MATCH '"`+term+`"'`))
	if fts != tri {
		t.Fatalf("the FTS lanes disagree on %q: unicode61 %d, trigram %d", term, fts, tri)
	}
	return fts
}

func continuityHome(t *testing.T, lines string) (*RwDb, string, string) {
	t.Helper()
	db, _ := indexTestDB(t)
	home := t.TempDir()
	rollout := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", lines)
	ingestForTest(t, home, db, 0)
	return db, home, rollout
}

// CRW-1083 (A8-02): a bigger, different file is not an append to the one that was indexed.
func TestIngestReplacementByALargerFileDropsTheOldMessages(t *testing.T) {
	for _, via := range []string{"in place", "new inode"} {
		t.Run(via, func(t *testing.T) {
			db, home, rollout := continuityHome(t, ingestMessage(t, "oldterm opening")+ingestLines(t, "shared", 0, 5))
			replacement := ingestMessage(t, "newterm opening") + ingestLines(t, "shared", 0, 40)
			if via == "in place" {
				ingestWrite(t, rollout, []byte(replacement))
			} else {
				next := filepath.Join(filepath.Dir(rollout), "next.tmp")
				ingestWrite(t, next, []byte(replacement))
				if err := os.Rename(next, rollout); err != nil {
					t.Fatal(err)
				}
			}
			r := ingestForTest(t, home, db, 0)
			if r.Ingested != 1 || r.Appended != 0 {
				t.Fatalf("a replacement must be indexed again, not appended: %+v", r)
			}
			if continuityHas(t, db, "oldterm") != 0 || continuityHas(t, db, "newterm") != 1 {
				t.Fatal("the old first message survived or the new one is missing")
			}
			ingestAssertConsistent(t, db)
			if f := ingestFresh(t, home, db, 0, nil); f.StaleFiles != 0 {
				t.Fatal(f)
			}
		})
	}
}

// The same size and the same millisecond mtime hide a rewrite from the metadata; the explicit
// strong path sees it, and the status says which of the two freshness meanings it reports.
func TestIngestVerifyCatchesASameMetadataRewrite(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestMessage(t, "oldterm opening")+ingestLines(t, "shared", 0, 5))
	before, _ := os.Stat(rollout)
	body, _ := os.ReadFile(rollout)
	rewritten := continuitySameSize(t, body, ingestMessage(t, "newterm opening")+ingestLines(t, "shared", 0, 5))
	if err := os.WriteFile(rollout, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rollout, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(rollout); after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the fixture must keep size and mtime")
	}
	if r := ingestForTest(t, home, db, 0); r.Ingested+r.Appended != 0 {
		t.Fatalf("the default refresh judges by metadata alone: %+v", r)
	}
	if f := ingestFresh(t, home, db, 0, nil); f.StaleFiles != 0 || f.Verified {
		t.Fatalf("metadata-only freshness cannot see the rewrite and must not claim to: %+v", f)
	}
	f, err := measureIndexFreshnessMode(home, db, 0, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Verified || f.ChangedFiles != 1 || f.StaleFiles != 1 {
		t.Fatalf("content-verified freshness missed the rewrite: %+v", f)
	}
	r, err := ingestWith(home, db, 0, ingestOptions{Verify: true})
	if err != nil || r.Ingested != 1 {
		t.Fatal(r, err)
	}
	if continuityHas(t, db, "oldterm") != 0 || continuityHas(t, db, "newterm") != 1 {
		t.Fatal("the strong path did not replace the rows")
	}
	ingestAssertConsistent(t, db)
	if f, _ := measureIndexFreshnessMode(home, db, 0, nil, true); f.StaleFiles != 0 || !f.Verified {
		t.Fatal(f)
	}
	// An untouched file costs the strong path no rebuild.
	if r, _ := ingestWith(home, db, 0, ingestOptions{Verify: true}); r.Ingested+r.Appended != 0 {
		t.Fatal(r)
	}
}

// continuitySameSize pads content with a blank line so that it has exactly the length of old.
func continuitySameSize(t *testing.T, old []byte, content string) []byte {
	t.Helper()
	pad := len(old) - len(content)
	if pad == 0 {
		return []byte(content)
	}
	if pad < 2 {
		t.Fatal("fixture content does not leave room for padding")
	}
	return []byte(content + strings.Repeat(" ", pad-1) + "\n")
}

// A different file with the same size and mtime (a copy put over the rollout) is another inode.
func TestIngestInodeReplacementWithTheSameMetadataIsRebuilt(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestMessage(t, "oldterm opening")+ingestLines(t, "shared", 0, 5))
	before, _ := os.Stat(rollout)
	body, _ := os.ReadFile(rollout)
	replacement := continuitySameSize(t, body, ingestMessage(t, "newterm opening")+ingestLines(t, "shared", 0, 5))
	next := filepath.Join(filepath.Dir(rollout), "next.tmp")
	if err := os.WriteFile(next, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, rollout); err != nil {
		t.Fatal(err)
	}
	if r := ingestForTest(t, home, db, 0); r.Ingested != 1 {
		t.Fatalf("a new inode is a new file: %+v", r)
	}
	if continuityHas(t, db, "oldterm") != 0 || continuityHas(t, db, "newterm") != 1 {
		t.Fatal("old rows survived an inode replacement")
	}
	ingestAssertConsistent(t, db)
}

func TestIngestTruncationDropsTheCutMessages(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestLines(t, "keep", 0, 5)+ingestMessage(t, "cutterm tail"))
	body, _ := os.ReadFile(rollout)
	cut := len(ingestLines(t, "keep", 0, 5))
	ingestWrite(t, rollout, body[:cut])
	if r := ingestForTest(t, home, db, 0); r.Ingested != 1 {
		t.Fatalf("%+v", r)
	}
	if continuityHas(t, db, "cutterm") != 0 {
		t.Fatal("a cut message stayed indexed")
	}
	ingestAssertConsistent(t, db)
	// Cut and then regrown past the old size with other content: still a different file.
	ingestWrite(t, rollout, []byte(ingestLines(t, "other", 0, 20)))
	if r := ingestForTest(t, home, db, 0); r.Ingested != 1 || r.Appended != 0 {
		t.Fatalf("%+v", r)
	}
	if continuityHas(t, db, "keep") != 0 {
		t.Fatal("old prefix survived a truncate and regrow")
	}
	ingestAssertConsistent(t, db)
}

// A line cut in the middle is not consumed; completing it is an append of exactly that message, once.
func TestIngestPartialLineAppendIsAppendedOnceCompleted(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestLines(t, "first", 0, 5))
	body, _ := os.ReadFile(rollout)
	line := ingestMessage(t, "partialterm message")
	ingestWrite(t, rollout, append(body, []byte(line[:len(line)/2])...))
	if r := ingestForTest(t, home, db, 0); r.Msgs != 0 {
		t.Fatalf("a partial line was consumed: %+v", r)
	}
	if continuityHas(t, db, "partialterm") != 0 {
		t.Fatal("partial line indexed")
	}
	grown, _ := os.ReadFile(rollout)
	ingestWrite(t, rollout, append(grown, []byte(line[len(line)/2:])...))
	if r := ingestForTest(t, home, db, 0); r.Appended != 1 || r.Ingested != 0 || r.Msgs != 1 {
		t.Fatalf("the completed line must be an append: %+v", r)
	}
	if continuityHas(t, db, "partialterm") != 1 {
		t.Fatal("completed line not indexed once")
	}
	ingestAssertConsistent(t, db)
}

// A genuine append is still an append: the checkpoint of the consumed prefix verifies.
func TestIngestGenuineAppendStaysAnAppend(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestLines(t, "first", 0, 300))
	for round := range 3 {
		body, _ := os.ReadFile(rollout)
		ingestWrite(t, rollout, append(body, []byte(ingestLines(t, "round", round*10, round*10+10))...))
		if r := ingestForTest(t, home, db, 0); r.Appended != 1 || r.Ingested != 0 || r.Msgs != 10 {
			t.Fatalf("round %d: %+v", round, r)
		}
	}
	ingestAssertConsistent(t, db)
}

// A cursor that no longer equals the committed rows is not continued.
func TestIngestCursorDisagreeingWithTheRowsRebuilds(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestLines(t, "first", 0, 10))
	recallSQL(t, db, "DELETE FROM msgs WHERE ord = 3")
	body, _ := os.ReadFile(rollout)
	ingestWrite(t, rollout, append(body, []byte(ingestLines(t, "grown", 0, 2))...))
	if r := ingestForTest(t, home, db, 0); r.Ingested != 1 || r.Appended != 0 {
		t.Fatalf("%+v", r)
	}
	ingestAssertConsistent(t, db)
}
