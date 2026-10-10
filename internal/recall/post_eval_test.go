package recall

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Post-evaluation round (31abc8f1): the findings of CRW-1154, 1083, 1087, 1089 and 1123 that were fixed.

// metaLineOfLength is a session_meta line of exactly n bytes (without its line break).
func metaLineOfLength(t *testing.T, n int) string {
	t.Helper()
	build := func(pad int) string {
		data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": "cap-thread", "cwd": "/proj/cap", "instructions": strings.Repeat("x", pad)}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	line := build(n - len(build(0)))
	if len(line) != n {
		t.Fatalf("fixture length %d, want %d", len(line), n)
	}
	return line
}

// CRW-1154, CRW-1089, CRW-1123 -- a complete first line that ends exactly at the read bound is complete.
func TestRolloutMetaLineEndingAtTheReadBound(t *testing.T) {
	const bound = 1_048_576
	for _, c := range []struct {
		name     string
		n        int
		tail     string
		complete bool
	}{
		{"below the bound", bound - 1, "\n", true},
		{"at the bound, line break follows", bound, "\n", true},
		{"at the bound, end of file follows", bound, "", true},
		{"one past the bound", bound + 1, "\n", false},
		{"at the bound, more content follows", bound, "x\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			content := metaLineOfLength(t, c.n) + c.tail
			if c.tail != "" {
				content += ingestMessage(t, "after meta")
			}
			path := writeRolloutTestFile(t, t.TempDir(), "r.jsonl", content)
			meta, err := ReadRolloutMeta(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.complete {
				if meta.Source != RolloutMain || meta.ThreadID == nil || *meta.ThreadID != "cap-thread" || meta.Cwd == nil {
					t.Fatalf("a complete metadata line was dropped: %+v", meta)
				}
			} else if meta.Source != RolloutUnknown {
				t.Fatalf("a line cut at the bound must be unknown: %+v", meta)
			}
		})
	}
}

// CRW-1154 d1 -- a refresh for one home removes the rows of another home, as the listing says they are
// not part of this one, even while the other home's files still exist.
func TestRefreshOfAnotherHomeDropsTheRowsOfTheFirst(t *testing.T) {
	db, _ := indexTestDB(t)
	homeA, homeB := t.TempDir(), t.TempDir()
	writeRolloutTestFile(t, homeA, "sessions/2026/01/01/a.jsonl", ingestMessage(t, "homeaterm opening"))
	writeRolloutTestFile(t, homeB, "sessions/2026/01/01/b.jsonl", ingestMessage(t, "homebterm opening"))
	ingestForTest(t, homeA, db, 0)
	if continuityHas(t, db, "homeaterm") != 1 {
		t.Fatal("setup")
	}
	r := ingestForTest(t, homeB, db, 0)
	if r.Pruned != 1 || continuityHas(t, db, "homeaterm") != 0 || continuityHas(t, db, "homebterm") != 1 {
		t.Fatalf("the other home's rows stayed: %+v", r)
	}
	ingestAssertConsistent(t, db)
	// A file of this home that is only missing from the listing for the moment still keeps its rows.
	home := t.TempDir()
	a := writeRolloutTestFile(t, home, "sessions/2026/01/02/c.jsonl", ingestMessage(t, "owncterm opening"))
	ingestForTest(t, home, db, 0)
	if underRolloutRoots(home, a) != true || underRolloutRoots(homeB, a) != false {
		t.Fatal("the home roots are wrong")
	}
}

// CRW-1083 d1, CRW-1089 d1, CRW-1123 d1 -- reading or counting the repeat history never resets the
// search index of an older schema; it stays as it is until a writer that ingests rebuilds it.
func TestSessionStartHistoryLeavesAnOlderSchemaIndexAlone(t *testing.T) {
	h := newAccountingHook(t, 3)
	recallSQL(t, h.db, "UPDATE meta SET value='2' WHERE key='schema_version'")
	store := hookContextOpenSidecarHitCounts(h.env)
	if store == nil {
		t.Fatal("the history store did not open")
	}
	if err := store.Bump("event-1", h.refs); err != nil {
		t.Fatal(err)
	}
	counts, err := store.Read(h.refs)
	if err != nil || counts["thread:t0"] != 1 {
		t.Fatalf("%v %v", counts, err)
	}
	_ = store.Close()
	var out bytes.Buffer
	h.run(&out)
	h.run(&out)
	if rows := indexRows(t, h.db, "SELECT COUNT(*) AS n FROM files"); rows[0]["n"] != float64(3) {
		t.Fatalf("the history reset the index: %v", rows)
	}
	if rows := indexRows(t, h.db, "SELECT COUNT(*) AS n FROM msgs"); rows[0]["n"] != float64(3) {
		t.Fatalf("the history reset the messages: %v", rows)
	}
	if rows := indexRows(t, h.db, "SELECT value FROM meta WHERE key='schema_version'"); rows[0]["value"] != "2" {
		t.Fatalf("the history migrated the schema: %v", rows)
	}
}

// CRW-1087 d2 -- an index written under another schema is not served to a reader: its repository keys were
// spelled by other rules. The search answers from the scan and says why; the index is not touched.
func TestOlderSchemaIndexIsNotServedToReaders(t *testing.T) {
	db, path := indexTestDB(t)
	home := t.TempDir()
	recallSQL(t, db, `INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES('/gone/x.jsonl',0,0,'t','/proj/other','main','2026-09-09','example.test/team/a%2Fb')`)
	recallSQL(t, db, `INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES('/gone/x.jsonl',0,'2026-09-09T00:00:00Z','user','content',0,'staleindexterm opening')`)
	recallSQL(t, db, "UPDATE meta SET value='2' WHERE key='schema_version'")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndexReadOnly(path); err == nil || !strings.Contains(err.Error(), "schema version 2") {
		t.Fatalf("%v", err)
	}
	if got := ListCwdSessions("/proj/other", 5, CwdSessionOptions{IndexPath: path, Home: home}); got != nil {
		t.Fatalf("the old index answered the cwd listing: %v", got)
	}
	days := float64(0)
	r, err := SearchChat("staleindexterm", ChatSearchOptions{Home: &home, IndexPath: &path, NoRefresh: true, Days: &days})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "scan" || len(r.Hits) != 0 || len(r.Warnings) == 0 || !strings.Contains(r.Warnings[0], "schema version 2") {
		t.Fatalf("%+v", r)
	}
	check, err := openIndexReadOnlyAnySchema(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if rows := indexRows(t, check, "SELECT COUNT(*) AS n FROM msgs"); rows[0]["n"] != float64(1) {
		t.Fatal("the reader changed the index")
	}
}

func unreadableDirHome(t *testing.T) (home, hidden, shown string) {
	t.Helper()
	home = t.TempDir()
	hiddenFile := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestMessage(t, "hiddenterm opening"))
	shown = writeRolloutTestFile(t, home, "sessions/2026/01/02/b.jsonl", ingestMessage(t, "shownterm opening"))
	return home, hiddenFile, shown
}

func makeUnreadable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("the user can read a mode-zero directory")
	}
}

// CRW-1083 d4, CRW-1087 d3, CRW-1089 d2, CRW-1123 d2 -- a directory that could not be listed is reported by the
// scan, the refresh and the status; no result of it claims to be complete.
func TestUnreadableDirectoryIsNeverReportedAsAComplete(t *testing.T) {
	db, path := indexTestDB(t)
	home, hidden, _ := unreadableDirHome(t)
	ingestForTest(t, home, db, 0)
	makeUnreadable(t, filepath.Dir(hidden))

	days := float64(0)
	scan, err := SearchChat("shownterm", ChatSearchOptions{Home: &home, Scan: true, Days: &days})
	if err != nil || len(scan.Hits) != 1 || !strings.Contains(strings.Join(scan.Warnings, "|"), "unreadable rollout directory: "+filepath.Dir(hidden)) {
		t.Fatalf("scan: %+v %v", scan, err)
	}
	r, err := ingest(home, db, 0)
	if err != nil || r.UnreadDirs != 1 || r.Pruned != 0 {
		t.Fatalf("ingest: %+v %v", r, err)
	}
	for _, verify := range []bool{false, true} {
		f, err := measureIndexFreshnessMode(home, db, 0, nil, verify)
		if err != nil || f.UnreadDirs != 1 || !f.Truncated || f.Verified {
			t.Fatalf("verify=%v: %+v %v", verify, f, err)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"chat", "index", "--home", home, "--index-path", path, "--status", "--verify", "--json"}, &out, &errOut, time.Now()); code != 0 {
		t.Fatalf("%d %s", code, errOut.String())
	}
	var report map[string]any
	_ = json.Unmarshal(out.Bytes(), &report)
	if report["freshness"] != "incomplete" || report["truncated"] != true || report["unreadDirs"] != float64(1) {
		t.Fatalf("%v", report)
	}
	res, err := SearchChat("shownterm", ChatSearchOptions{Home: &home, IndexPath: &path, NoRefresh: true, Days: &days})
	if err != nil || res.Mode != "index" || !strings.Contains(strings.Join(res.Warnings, "|"), "could not be listed") {
		t.Fatalf("index search: %+v %v", res, err)
	}
}

// CRW-1083 d3 -- the metadata-only status compares the file identity the index stores.
func TestStatusSeesAnInodeReplacementWithTheSameMetadata(t *testing.T) {
	db, home, rollout := continuityHome(t, ingestMessage(t, "oldterm opening")+ingestLines(t, "shared", 0, 5))
	before, _ := os.Stat(rollout)
	body, _ := os.ReadFile(rollout)
	next := filepath.Join(filepath.Dir(rollout), "next.tmp")
	if err := os.WriteFile(next, continuitySameSize(t, body, ingestMessage(t, "newterm opening")+ingestLines(t, "shared", 0, 5)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, rollout); err != nil {
		t.Fatal(err)
	}
	if f := ingestFresh(t, home, db, 0, nil); f.ChangedFiles != 1 || f.StaleFiles != 1 || f.Verified {
		t.Fatalf("the status missed the replaced file: %+v", f)
	}
	if r := ingestForTest(t, home, db, 0); r.Ingested != 1 {
		t.Fatal(r)
	}
	if f := ingestFresh(t, home, db, 0, nil); f.StaleFiles != 0 {
		t.Fatalf("%+v", f)
	}
}
