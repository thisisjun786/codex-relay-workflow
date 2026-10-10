package recall

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// CRW-1123: the storage and collection sweep of the recall port. One test per item of the issue.

func sweepEnv(values map[string]string) host.LookupEnv {
	return func(k string) (string, bool) { v, ok := values[k]; return v, ok }
}

// known-defects.md :69 -- an empty HOME names no home; nothing is created relative to the workspace.
func TestSweepEmptyHomeIsRefused(t *testing.T) {
	env := sweepEnv(map[string]string{"HOME": ""})
	if got, err := indexPath(env); err == nil {
		t.Fatalf("index path %q from an empty HOME", got)
	}
	if got, err := codexHome(env); err == nil {
		t.Fatalf("codex home %q from an empty HOME", got)
	}
	if got, err := hookContextHome(env); err == nil {
		t.Fatalf("hook home %q from an empty HOME", got)
	}
	// A home that is named is used; an unset HOME falls to the account's home, as before.
	if got, err := indexPath(sweepEnv(map[string]string{"HOME": "", "CRW_HOME": "/named"})); err != nil || got != filepath.Join("/named", "recall", "index.sqlite") {
		t.Fatal(got, err)
	}
	if got, err := codexHome(sweepEnv(map[string]string{"HOME": "", "CODEX_HOME": "/named"})); err != nil || got != "/named" {
		t.Fatal(got, err)
	}
}

// :390 -- a directory or a link that is no database does not hide the usable one.
func TestSweepVersionedDBNeedsAUsableFile(t *testing.T) {
	root := t.TempDir()
	write := func(name string) { writeRolloutTestFile(t, root, name, "") }
	if err := os.Mkdir(filepath.Join(root, "state_99.sqlite"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(root, "state_98.sqlite")); err != nil {
		t.Fatal(err)
	}
	write("state_3.sqlite")
	if got, err := stateDbPath(root); err != nil || got != filepath.Join(root, "state_3.sqlite") {
		t.Fatalf("a directory or a dangling link hid the database: %q, %v", got, err)
	}
	// A link to a regular file inside the home is a usable database.
	write("real.sqlite")
	if err := os.Symlink(filepath.Join(root, "real.sqlite"), filepath.Join(root, "state_5.sqlite")); err != nil {
		t.Fatal(err)
	}
	if got, err := stateDbPath(root); err != nil || got != filepath.Join(root, "state_5.sqlite") {
		t.Fatalf("a link to a file is a database: %q, %v", got, err)
	}
	onlyDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(onlyDir, "state_1.sqlite"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := stateDbPath(onlyDir); err != nil || got != "" {
		t.Fatalf("a directory is no database: %q, %v", got, err)
	}
}

// :391 -- a home that goes through a link and `..` names the directory the system lists.
func TestSweepVersionedDBHomeIsResolvedOnce(t *testing.T) {
	root := t.TempDir()
	outer, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outer, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRolloutTestFile(t, outer, "state_7.sqlite", "")
	if err := os.Symlink(filepath.Join(outer, "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := stateDbPath(root + "/link/..")
	if err != nil || got != filepath.Join(outer, "state_7.sqlite") {
		t.Fatalf("listed %s but joined a different directory: %q, %v", outer, got, err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatal("the named database does not exist", err)
	}
}

// :392 -- a URI cannot turn a read-only open into a writable or an in-memory one.
func TestSweepReadOnlyRefusesURIOverrides(t *testing.T) {
	for _, uri := range []string{"file:recall-memory?mode=memory", "file:x?mode=rwc", "file:x?mode=rw", "file:x?cache=shared&mode=memory"} {
		if d, err := openDbReadOnly(uri); err == nil {
			_ = d.Close()
			t.Errorf("%s opened", uri)
		}
	}
	p := filepath.Join(t.TempDir(), "ok.sqlite")
	d := recallDB(t, p)
	recallSQL(t, d, "CREATE TABLE t(x)")
	_ = d.Close()
	ro, err := openDbReadOnly("file:" + p + "?mode=ro")
	if err != nil {
		t.Fatal("a read-only URI is the open that was asked for:", err)
	}
	_ = ro.Close()
}

// :393 -- only a missing legacy column sends the reader to the shorter query.
func TestSweepThreadMetaRetriesOnlyAMissingLegacyColumn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.sqlite")
	d := recallDB(t, p)
	recallSQL(t, d, `CREATE TABLE threads(id,title,cwd,git_branch,git_origin_url,updated_at_ms);INSERT INTO threads VALUES ('x','Origin','/p',NULL,9007199254740992,1)`)
	_ = d.Close()
	r := loadThreadMeta(p)
	if r.Warning == "" || len(r.ByID) != 0 {
		t.Fatalf("a damaged row was dropped without a word: %+v", r)
	}
	p = filepath.Join(t.TempDir(), "legacy.sqlite")
	d = recallDB(t, p)
	recallSQL(t, d, `CREATE TABLE threads(id,title,cwd,git_branch,updated_at_ms);INSERT INTO threads VALUES ('x','Legacy','/old',NULL,5)`)
	_ = d.Close()
	if r = loadThreadMeta(p); r.Warning != "" || r.ByID["x"].Title != "Legacy" {
		t.Fatalf("a database without the column is a legacy one: %+v", r)
	}
}

// :394 -- the columns are the ones the query names, whatever case the table declared.
func TestSweepThreadMetaColumnNamesIgnoreCase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.sqlite")
	d := recallDB(t, p)
	recallSQL(t, d, `CREATE TABLE threads(ID,TITLE,CWD,GIT_BRANCH,GIT_ORIGIN_URL,UPDATED_AT_MS);INSERT INTO threads VALUES ('x','Title','/p','main','https://example.test/a/b.git',1)`)
	_ = d.Close()
	r := loadThreadMeta(p)
	got, ok := r.ByID["x"]
	if r.Warning != "" || !ok || got.Title != "Title" || got.Cwd != "/p" || got.GitBranch == nil || *got.GitBranch != "main" || got.GitOriginURL == nil {
		t.Fatalf("rows vanished behind the declared case: %+v", r)
	}
}

// :396 -- an ambiguous bare name is refused every time, never half-remembered.
func TestSweepAmbiguousAliasIsNotCached(t *testing.T) {
	d := recallDB(t, ":memory:")
	a := recallStmt(t, d, "SELECT :x AS x,@x AS y")
	for i := range 3 {
		if _, err := a.Get(NamedParams{{"x", 2}}); err == nil {
			t.Fatalf("call %d: the ambiguous name bound the first alias", i+1)
		}
	}
}

// :397 -- a name with a NUL byte is refused, not read up to the NUL.
func TestSweepNamedKeyWithNULIsRefused(t *testing.T) {
	d := recallDB(t, ":memory:")
	s := recallStmt(t, d, "SELECT $x AS x")
	for _, key := range []string{"x\x00tail", "$x\x00tail"} {
		if _, err := s.Get(NamedParams{{key, 7}}); err == nil || !strings.Contains(err.Error(), "null") {
			t.Errorf("%q: %v", key, err)
		}
	}
	if _, err := s.Get(map[string]any{"x\x00tail": 7}); err == nil {
		t.Error("map key with NUL accepted")
	}
}

// :448 -- a local drive path is not an scp remote.
func TestSweepLocalDrivePathIsNoRemote(t *testing.T) {
	for _, raw := range []string{`C:\repo`, `C:/repo`, `d:\work\repo.git`, `Z:/x`} {
		if got := normalizeRepoKey(raw); got != "" {
			t.Errorf("%q identifies %q", raw, got)
		}
	}
	for raw, want := range map[string]string{"host:repo": "host/repo", "git@github.com:a/b.git": "github.com/a/b", "gh:a/b": "gh/a/b"} {
		if got := normalizeRepoKey(raw); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}

// :449 -- an encoded separator stays inside its path segment.
func TestSweepEncodedSeparatorKeepsItsSegment(t *testing.T) {
	plain := normalizeRepoKey("https://example.test/a/b")
	for _, raw := range []string{"https://example.test/a%2Fb", "https://example.test/a%2fb", "https://example.test/a%5Cb"} {
		got := normalizeRepoKey(raw)
		if got == "" || got == plain {
			t.Errorf("%q merged with a/b: %q", raw, got)
		}
	}
	if a, b := normalizeRepoKey("https://example.test/a%2Fb"), normalizeRepoKey("https://example.test/a%2fb"); a != b {
		t.Errorf("one separator, two keys: %q %q", a, b)
	}
	if got := normalizeRepoKey("https://example.test/caf%C3%A9"); got != "example.test/café" {
		t.Error(got)
	}
}

func sweepSession(t *testing.T, home, day, name, body string) string {
	t.Helper()
	return writeRolloutTestFile(t, home, filepath.Join("sessions", day, name), body)
}

func sweepMessage(t *testing.T, text string) string {
	return rolloutTestLine(t, map[string]any{"type": "response_item", "timestamp": "2026-08-20T00:00:00.000Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}})
}

// :499 -- a directory named *.jsonl is no rollout, and one unusable entry does not end the listing.
func TestSweepUnusableEntriesAreFilteredPerFile(t *testing.T) {
	home := t.TempDir()
	sweepSession(t, home, "2026/08/20", "good.jsonl", sweepMessage(t, "findable needle"))
	if err := os.MkdirAll(filepath.Join(home, "sessions", "2026", "08", "20", "dir.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, "sessions", "2026", "08", "20", "dangling.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "archived_sessions", "rollout-2026-08-19T01-00-00-dir.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := ListRolloutFiles(home, 0)
	if err != nil || len(files) != 1 || !strings.HasSuffix(files[0].Path, "good.jsonl") {
		t.Fatalf("listing %v, %v", files, err)
	}
	days := float64(0)
	r, err := SearchChat("needle", ChatSearchOptions{Home: &home, Scan: true, Days: &days})
	if err != nil || len(r.Hits) != 1 {
		t.Fatalf("one directory ended the scan: %+v, %v", r.Hits, err)
	}
	// An unreadable file is skipped by the refresh and the rest is indexed.
	bad := sweepSession(t, home, "2026/08/20", "unreadable.jsonl", sweepMessage(t, "never read"))
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	if f, err := os.Open(bad); err == nil {
		_ = f.Close()
		t.Skip("permission bits do not stop this user")
	}
	db, _ := indexTestDB(t)
	res, err := ingest(home, db, 0)
	if err != nil || res.Msgs != 1 {
		t.Fatalf("refresh with one unreadable file: %+v, %v", res, err)
	}
	r, err = SearchChat("needle", ChatSearchOptions{Home: &home, Scan: true, Days: &days})
	if err != nil || len(r.Hits) != 1 {
		t.Fatalf("scan with one unreadable file: %+v, %v", r.Hits, err)
	}
}

// :501 -- days are validated once, and a window wider than the calendar prunes nothing.
func TestSweepDaysAndFolderDatesAreValidated(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	sweepSession(t, home, "2026/08/20", "a.jsonl", "")
	sweepSession(t, home, "2025/01/01", "b.jsonl", "")
	sweepSession(t, home, "2026/13/45", "c.jsonl", "")
	for _, days := range []float64{math.Inf(1), 1e20, 1e300} {
		files, err := ListRolloutFiles(home, days, now)
		if err != nil || len(files) < 2 {
			t.Errorf("days %v pruned every source: %v, %v", days, files, err)
		}
	}
	files, err := ListRolloutFiles(home, 3, now)
	if err != nil || len(files) != 1 || !strings.HasSuffix(files[0].Path, "a.jsonl") {
		t.Fatalf("a folder that is no date has no age to be inside a window: %v, %v", files, err)
	}
	if all, err := ListRolloutFiles(home, 0, now); err != nil || len(all) != 3 {
		t.Fatalf("without a window the invalid folder is still listed: %v, %v", all, err)
	}
}

// :502 -- a first line past the read bound cannot say what the session is: it is unknown, not main.
func TestSweepOversizedMetaLineIsUnknown(t *testing.T) {
	home := t.TempDir()
	huge := `{"type":"session_meta","payload":{"id":"big","thread_source":"subagent","instructions":"` + strings.Repeat("x", 1_100_000) + `"}}` + "\n"
	path := sweepSession(t, home, "2026/08/20", "big.jsonl", huge+sweepMessage(t, "oversized needle"))
	meta, err := ReadRolloutMeta(path)
	if err != nil || string(meta.Source) != "unknown" || meta.ThreadID != nil {
		t.Fatalf("%+v, %v", meta, err)
	}
	sweepSession(t, home, "2026/08/20", "small.jsonl", rolloutTestLine(t, map[string]any{"type": "session_meta", "payload": map[string]any{"id": "small"}})+sweepMessage(t, "oversized needle"))
	days := float64(0)
	r, err := SearchChat("needle", ChatSearchOptions{Home: &home, Scan: true, Days: &days})
	if err != nil || len(r.Hits) != 1 || r.Hits[0].ThreadID == nil || *r.Hits[0].ThreadID != "small" {
		t.Fatalf("the main-only scan returned the unknown file: %+v, %v", r.Hits, err)
	}
	all := RolloutAll
	if r, err = SearchChat("needle", ChatSearchOptions{Home: &home, Scan: true, Days: &days, Source: &all}); err != nil || len(r.Hits) != 2 {
		t.Fatalf("--source all still sees it: %+v, %v", r.Hits, err)
	}
	db, indexPath := indexTestDB(t)
	_ = indexPath
	if _, err := ingest(home, db, 0); err != nil {
		t.Fatal(err)
	}
	r, err = SearchChat("needle", ChatSearchOptions{Home: &home, IndexPath: &indexPath, Days: &days, NoRefresh: true})
	if err != nil || len(r.Hits) != 1 {
		t.Fatalf("the main-only index search returned the unknown file: %+v, %v", r.Hits, err)
	}
}

// :505 -- an entry of a type the parser cannot read is dropped alone.
func TestSweepUnreadableEntryIsIsolated(t *testing.T) {
	good := sweepMessage(t, "kept before") + sweepMessage(t, "kept after")
	bad := `{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":{"toString":null}}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","name":{"toString":1},"arguments":"{}"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","name":"tool","arguments":{"toString":1}}}` + "\n"
	entries, err := ParseRollout(sweepMessage(t, "kept before")+bad+sweepMessage(t, "kept after"), true)
	if err != nil || len(entries) != 2 || entries[0].Text != "kept before" || entries[1].Text != "kept after" {
		t.Fatalf("%+v, %v", entries, err)
	}
	_ = good
}

// :506 -- a match that the file spells with escapes is found: the decoded text decides.
func TestSweepEscapedMatchesAreDecodedBeforeTheyAreJudged(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","timestamp":"2026-08-20T00:00:00.000Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run \u0043I now"}]}}` + "\n"
	sweepSession(t, home, "2026/08/20", "esc.jsonl", line)
	days := float64(0)
	r, err := SearchChat("CI", ChatSearchOptions{Home: &home, Scan: true, Days: &days})
	if err != nil || len(r.Hits) != 1 || !strings.Contains(r.Hits[0].Text, "run CI now") {
		t.Fatalf("the raw prefilter dropped an escaped match: %+v, %v", r.Hits, err)
	}
	typed := strings.Replace(line, "response_item", `response_\u0069tem`, 1)
	entries, err := ParseRollout(typed, false)
	if err != nil || len(entries) != 1 {
		t.Fatalf("an escaped type is the same type: %+v, %v", entries, err)
	}
}

// :506 -- every JSON escape, not only \u, is decoded before the raw prefilter judges a file:
// a scan finds what the index finds for a backslash, a quote, a slash and a control character.
func TestSweepEveryJSONEscapeMatchesLikeTheIndex(t *testing.T) {
	cases := []struct{ name, text, query string }{
		{"backslash", `open C:\repo now`, `C:\repo`},
		{"quote", `the say"so flag`, `say"so`},
		{"slash", `escaped \/ slash in a/b path`, `a/b`},
		{"tab", "tab\tseparated", "tab separated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			line := sweepMessage(t, c.text)
			if c.name == "slash" {
				line = strings.Replace(line, "a/b", `a\/b`, 1)
			}
			sweepSession(t, home, "2026/08/20", "a.jsonl", line)
			days := float64(0)
			scan, err := SearchChat(c.query, ChatSearchOptions{Home: &home, Scan: true, Days: &days})
			if err != nil {
				t.Fatal(err)
			}
			db, path := indexTestDB(t)
			ingestForTest(t, home, db, 0)
			indexed, err := SearchChat(c.query, ChatSearchOptions{Home: &home, IndexPath: &path, NoRefresh: true, Days: &days})
			if err != nil {
				t.Fatal(err)
			}
			if len(scan.Hits) != 1 || len(indexed.Hits) != 1 {
				t.Fatalf("%s: scan=%d index=%d", c.query, len(scan.Hits), len(indexed.Hits))
			}
		})
	}
}

func TestPrefilterTextDecodesJSONEscapes(t *testing.T) {
	for raw, want := range map[string]string{
		`a\\b`:            `a\b`,
		`q\"x\/y`:         `q"x/y`,
		`\u0043I \\u0043`: `CI \u0043`,
		`\ud83d\ude00!`:   "\U0001F600!",
		`lone \ud83d end`: "lone \uFFFD end",
		`\n\t\r\b\f`:      "\n\t\r\b\f",
		`bad \x and \u12`: `bad \x and \u12`,
		`trailing \`:      `trailing \`,
		`plain text`:      `plain text`,
	} {
		if got := prefilterText(raw); got != want {
			t.Errorf("prefilterText(%q) = %q, want %q", raw, got, want)
		}
	}
}
