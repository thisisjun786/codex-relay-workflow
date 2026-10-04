package recall

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func chatSearchFixture(t *testing.T) (string, string) {
	t.Helper()
	home := buildRecallCodexHome(t, scanTestNow())
	t.Setenv("HOME", home)
	t.Setenv("CRW_HOME", t.TempDir())
	return home, filepath.Join(home, "sidecar", "index.sqlite")
}

func chatSearchRun(t *testing.T, home, index, query string, opts ChatSearchOptions) ChatSearchResult {
	t.Helper()
	opts.Home, opts.IndexPath = &home, &index
	if opts.NowMs == nil {
		opts.NowMs = scanPtr(float64(scanTestNow().UnixMilli()))
	}
	r, err := SearchChat(query, opts, scanTestNow())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// chat-search.test.ts:32-135, driven through the public entry rather than
// changing the already-landed direct scan tests. Index uses recent ordering.
func TestChatSearchEntryCases(t *testing.T) {
	home, index := chatSearchFixture(t)
	chatSearchRun(t, home, index, "trigram", ChatSearchOptions{})
	for _, tc := range []struct {
		name, query string
		opts        ChatSearchOptions
		count       int
	}{
		{"archive", "aardwolf", ChatSearchOptions{Days: scanPtr(0.0)}, 2},
		{"archive-pruned", "aardwolf", ChatSearchOptions{}, 0},
		{"and", "trigram korean", ChatSearchOptions{Source: scanPtr(RolloutAll)}, 2},
		{"or", "trigram korean", ChatSearchOptions{Source: scanPtr(RolloutAll), Any: true}, 3},
		{"main", "trigram", ChatSearchOptions{}, 2},
		{"subagent", "trigram", ChatSearchOptions{Source: scanPtr(RolloutSubagent)}, 1},
		{"synthetic-hidden", "zebra", ChatSearchOptions{Role: scanPtr("user")}, 0},
		{"synthetic-shown", "zebra", ChatSearchOptions{Role: scanPtr("user"), IncludeSynthetic: true}, 1},
		{"tools", "zebra-in-tool-output", ChatSearchOptions{}, 1},
		{"no-tools", "zebra-in-tool-output", ChatSearchOptions{IncludeTools: scanPtr(false)}, 0},
		{"old-hidden", "ancient question", ChatSearchOptions{}, 0},
		{"all-days", "ancient question", ChatSearchOptions{Days: scanPtr(0.0)}, 1},
		{"cwd", "trigram", ChatSearchOptions{Source: scanPtr(RolloutAll), Cwd: scanPtr("/proj/alph"), ReadOriginUrl: func(string) string { return "" }}, 0},
		{"korean", "트라이그램", ChatSearchOptions{}, 2},
		{"context", "한글 트라이그램 결과", ChatSearchOptions{Context: scanPtr(1.0)}, 1},
		{"role-limit", "trigram", ChatSearchOptions{Source: scanPtr(RolloutAll), Role: scanPtr("user"), Limit: scanPtr(1.0)}, 1},
		{"short-like", "한글", ChatSearchOptions{}, 2},
		{"fts-quotes", `trigram "index`, ChatSearchOptions{Any: true}, 2},
		{"relaxed-required", "please deploy the trigram index for korean search extra", ChatSearchOptions{Source: scanPtr(RolloutAll)}, 1},
		{"relaxed-optional", "please deploy trigram index korean search 확인 완료 없는단어", ChatSearchOptions{Source: scanPtr(RolloutAll)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Scan = true
			scan := chatSearchRun(t, home, index, tc.query, opts)
			if len(scan.Hits) != tc.count {
				t.Fatalf("scan count %d want %d: %+v", len(scan.Hits), tc.count, scan)
			}
			opts.Scan, opts.NoRefresh, opts.Order = false, true, ChatRecent
			indexed := chatSearchRun(t, home, index, tc.query, opts)
			if indexed.Mode != "index" || !reflect.DeepEqual(indexed.Hits, scan.Hits) {
				t.Fatalf("entry index/scan differ: %+v\n%+v", indexed, scan)
			}
		})
	}
	r := chatSearchRun(t, home, index, "트라이그램", ChatSearchOptions{Scan: true})
	if scanString(r.Hits[0].Title) != "deploy trigram index" || scanString(r.Hits[0].GitBranch) != "main" {
		t.Fatal(r)
	}
	r = chatSearchRun(t, home, index, "한글 트라이그램 결과", ChatSearchOptions{Context: scanPtr(1.0)})
	if len(r.Hits[0].Context) != 3 || !r.Hits[0].Context[1].IsMatch {
		t.Fatal(r)
	}
}

func TestChatSearchDispatchAndNumericEdges(t *testing.T) {
	home, index := chatSearchFixture(t)
	for _, q := range []string{"", " "} {
		r := chatSearchRun(t, home, index, q, ChatSearchOptions{})
		if r.Mode != "scan" || !reflect.DeepEqual(r.Warnings, []string{"empty query"}) {
			t.Fatal(r)
		}
	}
	chatSearchRun(t, home, index, "trigram", ChatSearchOptions{Scan: true})
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatal("empty/forced scan created sidecar", err)
	}
	// Short ASCII terms remain required symbols, even when they are stopwords.
	if r := chatSearchRun(t, home, index, "the and", ChatSearchOptions{}); r.Mode != "index" || len(r.Hits) != 0 {
		t.Fatal(r)
	}
	calls := 0
	_, err := SearchChat("", ChatSearchOptions{Home: &home, Days: scanPtr(math.Inf(1)), Cwd: scanPtr("/proj/alpha"), ReadOriginUrl: func(string) string { calls++; return "" }}, scanTestNow())
	if err == nil || err.Error() != "Invalid time value" || calls != 0 {
		t.Fatalf("cutoff must precede empty query and origin: %v calls=%d", err, calls)
	}
	for _, n := range []float64{0, -1} {
		r := chatSearchRun(t, home, index, "trigram", ChatSearchOptions{Scan: true, Limit: &n, Context: &n})
		if len(r.Hits) != 1 || len(r.Hits[0].Context) != 0 {
			t.Fatal(r)
		}
	}
	r := chatSearchRun(t, home, index, "ancient", ChatSearchOptions{Days: scanPtr(math.NaN())})
	if len(r.Hits) != 2 {
		t.Fatal(r)
	}
	// Default path/home are resolved from isolated environment, not test overrides.
	r, err = SearchChat("trigram", ChatSearchOptions{NowMs: scanPtr(float64(scanTestNow().UnixMilli()))}, scanTestNow())
	if err != nil || r.Mode != "index" || len(r.Hits) != 2 {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	defaultIndex, err := indexPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaultIndex); err != nil {
		t.Fatal(err)
	}
}

// index.test.ts:182-217; index-freshness.test.ts:273-282; round2:31-55.
func TestChatSearchRefreshAndFreshness(t *testing.T) {
	home, index := chatSearchFixture(t)
	cold := chatSearchRun(t, home, index, "trigram", ChatSearchOptions{})
	if cold.ScannedFiles != 4 || cold.Index == nil || cold.Index.ReadOnly || cold.Index.Files != 4 || cold.Index.StaleFiles != 0 || cold.Index.LastIngestAt == nil {
		t.Fatal(cold)
	}
	warm := chatSearchRun(t, home, index, "trigram", ChatSearchOptions{})
	if warm.ScannedFiles != 0 {
		t.Fatal(warm)
	}
	files, err := ListRolloutFiles(home, 0, scanTestNow())
	if err != nil {
		t.Fatal(err)
	}
	var main string
	for _, f := range files {
		if strings.Contains(f.Path, recallThreadMain) {
			main = f.Path
		}
	}
	f, err := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(recallFixtureMessage(t, "user", "freshly appended xylophone question", scanTestNow().Format("2006-01-02T15:04:05.000Z")))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	bumped := scanTestNow().Add(2 * time.Second)
	if err := os.Chtimes(main, bumped, bumped); err != nil {
		t.Fatal(err)
	}
	stale := chatSearchRun(t, home, index, "xylophone", ChatSearchOptions{NoRefresh: true})
	if len(stale.Hits) != 0 || stale.Index == nil || !stale.Index.ReadOnly || stale.Index.StaleFiles != 1 || stale.Index.SourceFiles != stale.Index.Files || stale.ScannedFiles != 0 {
		t.Fatal(stale)
	}
	fresh := chatSearchRun(t, home, index, "xylophone", ChatSearchOptions{})
	if len(fresh.Hits) != 1 || fresh.ScannedFiles != 1 || fresh.Index.StaleFiles != 0 || fresh.Index.ReadOnly {
		t.Fatal(fresh)
	}
	// The index caps tools; the raw scan continues to see the complete output.
	path := recallFixtureRollout(t, home, scanTestNow(), 0, "03", "cap", "/proj/alpha", "", false, false, nil)
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(recallFixtureTool(t, "exec_command", "{}", strings.Repeat("y", TOOL_TEXT_CAP+500)+" needleinbigoutput", scanTestNow().Format(time.RFC3339)))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(chatSearchRun(t, home, index, "needleinbigoutput", ChatSearchOptions{}).Hits) != 0 || len(chatSearchRun(t, home, index, "needleinbigoutput", ChatSearchOptions{Scan: true}).Hits) != 1 {
		t.Fatal("tool cap parity")
	}
}

func TestChatSearchFallbacks(t *testing.T) {
	home, index := chatSearchFixture(t)
	for _, path := range []string{index, filepath.Join(home, "sessions")} {
		r := chatSearchRun(t, home, path, "trigram", ChatSearchOptions{NoRefresh: true})
		if r.Mode != "scan" || len(r.Hits) != 2 || !strings.HasPrefix(r.Warnings[0], "index unavailable (") {
			t.Fatal(r)
		}
	}
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatal("NoRefresh created index")
	}
	r := chatSearchRun(t, home, filepath.Join(home, "sessions"), "trigram", ChatSearchOptions{})
	if r.Mode != "scan" || len(r.Hits) != 2 {
		t.Fatal(r)
	}
	bare := t.TempDir()
	r = chatSearchRun(t, bare, filepath.Join(bare, "missing.sqlite"), "anything", ChatSearchOptions{NoRefresh: true})
	if len(r.Warnings) != 2 || !strings.HasPrefix(r.Warnings[0], "index unavailable") || !strings.Contains(r.Warnings[1], "state db") {
		t.Fatal(r)
	}
}

func chatSearchConfigureIndex(t *testing.T, index, sql string) {
	t.Helper()
	db, err := openIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Exec(sql + "; PRAGMA wal_checkpoint(TRUNCATE)")
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func chatSearchMakeReadOnly(t *testing.T, index string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("write-protection witness requires a non-root host")
	}
	chatSearchConfigureIndex(t, index, "UPDATE meta SET value='1' WHERE key='schema_version'")
	if err := os.Chmod(index, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(index, 0o600) })
}

func TestChatSearchReadOnlyDegradation(t *testing.T) {
	home, index := chatSearchFixture(t)
	chatSearchRun(t, home, index, "trigram", ChatSearchOptions{})
	chatSearchMakeReadOnly(t, index)
	r := chatSearchRun(t, home, index, "trigram", ChatSearchOptions{})
	if r.Mode != "index" || len(r.Hits) != 2 || !r.Index.ReadOnly || r.ScannedFiles != 0 || !slices.Contains(r.Warnings, "index opened read-only (attempt to write a readonly database) — refresh skipped") {
		t.Fatal(r)
	}
}

// index.test.ts:272-440: origin lookup once, prefix safety and NULL repo keys.
func TestChatSearchCwdAndOrigin(t *testing.T) {
	home, index := chatSearchFixture(t)
	for _, tc := range []struct {
		cwd, origin string
		count       int
	}{
		{"/proj/alpha", "", 4}, {"/proj/beta", "", 1}, {"/different/alpha", recallOriginAlpha, 3}, {"/proj/alph", "", 0},
	} {
		for _, scan := range []bool{false, true} {
			calls := 0
			r := chatSearchRun(t, home, index, "trigram", ChatSearchOptions{Days: scanPtr(0.0), Source: scanPtr(RolloutAll), Scan: scan, Cwd: &tc.cwd, ReadOriginUrl: func(string) string { calls++; return tc.origin }})
			if len(r.Hits) != tc.count || calls != 1 {
				t.Fatalf("%+v scan=%t calls=%d: %+v", tc, scan, calls, r)
			}
		}
	}
	for _, tc := range []struct {
		recorded, query string
		count           int
	}{
		{`\\?\C:\Users\super\Developers`, `C:/Users/super/Developers`, 1},
		{`\\?\C:\Users\super\Developers`, `C:\Users\super\Developers`, 1},
		{`C:\Users\super\Developers`, `C:/Users/super/Developers`, 1},
		{`C:\Users\super\Developers`, `C:\Users\super\Developers2`, 0},
		{`C:\Users\super\Developers\sub`, `C:/Users/super/Developers`, 1},
		{`\\?\C:\Users\super\Developers\sub`, `C:\Users\super\Developers`, 1},
	} {
		t.Run(tc.recorded+tc.query, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "index.sqlite")
			recallFixtureRollout(t, root, scanTestNow(), 0, "01", "null-repo", tc.recorded, "", false, false, [][2]string{{"user", "lidgejun-child"}})
			for _, scan := range []bool{false, true} {
				r := chatSearchRun(t, root, path, "lidgejun", ChatSearchOptions{Scan: scan, Cwd: &tc.query, ReadOriginUrl: func(string) string { return "" }})
				if len(r.Hits) != tc.count || tc.count > 0 && scanString(r.Hits[0].Cwd) != tc.recorded {
					t.Fatal(r)
				}
			}
		})
	}
	root := t.TempDir()
	path := filepath.Join(root, "index.sqlite")
	recallFixtureRollout(t, root, scanTestNow(), 0, "01", "case", `C:\Users\super\Developers`, "", false, false, [][2]string{{"user", "lidgejun"}})
	count := 0
	if FoldCwdCase() {
		count = 1
	}
	for _, scan := range []bool{false, true} {
		r := chatSearchRun(t, root, path, "lidgejun", ChatSearchOptions{Scan: scan, Cwd: scanPtr(`c:\users\super\developers`), ReadOriginUrl: func(string) string { return "" }})
		if len(r.Hits) != count {
			t.Fatal("platform case folding", r)
		}
	}
}

// index-rank.test.ts:126-189 and hit-count.test.ts:338-361 through SearchChat.
func TestChatSearchRankAndHistory(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	index := filepath.Join(home, "sidecar", "index.sqlite")
	for _, query := range []string{"quokka", "wombat", "트라이그램", "한글"} {
		opts := ChatSearchOptions{Days: scanPtr(0.0), NowMs: &now, NoRefresh: true}
		before := chatSearchRun(t, home, index, query, opts)
		expected := indexRankSearch(t, db, indexRankOptions(home, query, now))
		if !reflect.DeepEqual(before.Hits, expected.Hits) {
			t.Fatalf("NowMs/options plumbing: %s", query)
		}
		opts.Order = ChatRecent
		recent := chatSearchRun(t, home, index, query, opts)
		if !reflect.DeepEqual(indexRankKeys(before.Hits), indexRankKeys(recent.Hits)) {
			t.Fatal("ordering changed hit set")
		}
		if query == "quokka" && (!strings.Contains(before.Hits[0].Text, "quokka deployment plan") || !strings.Contains(recent.Hits[0].Text, "quokka reference at the end") || recent.Hits[0].Score != nil) {
			t.Fatal(before, recent)
		}
		opts.Order = ChatRelevance
		repeated := chatSearchRun(t, home, index, query, opts)
		if !reflect.DeepEqual(before.Hits, repeated.Hits) {
			t.Fatal("nondeterministic ranked query")
		}
		if query == "wombat" && (len(before.Hits) != 2 || before.Hits[0].TS <= before.Hits[1].TS || *before.Hits[0].Score <= *before.Hits[1].Score) {
			t.Fatal("recency tie break", before)
		}
		if query == "트라이그램" && !strings.Contains(before.Hits[0].Text, "트라이그램 색인") {
			t.Fatal("Korean ranking", before)
		}
		if query == "한글" {
			opts.Scan = true
			if len(before.Hits) == 0 || len(chatSearchRun(t, home, index, query, opts).Hits) != len(before.Hits) {
				t.Fatal("short Korean LIKE parity")
			}
		}
	}
	limited := chatSearchRun(t, home, index, "quokka", ChatSearchOptions{Days: scanPtr(0.0), NowMs: &now, Limit: scanPtr(1.0), NoRefresh: true})
	if len(limited.Hits) != 1 || !strings.Contains(limited.Hits[0].Text, "quokka deployment plan") || !slices.Contains(limited.Warnings, "truncated at limit 1 — raise --limit or narrow the query") {
		t.Fatal(limited)
	}
	home, index = chatSearchFixture(t)
	opts := ChatSearchOptions{Source: scanPtr(RolloutAll)}
	before := chatSearchRun(t, home, index, "trigram", opts)
	history, err := openIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	refs := []string{}
	for _, h := range before.Hits {
		refs = append(refs, hitCountRef(scanString(h.ThreadID), h.File))
	}
	for i := 0; i < 20; i++ {
		if err := bumpHitCounts(history, refs, "2026-09-09T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	after := chatSearchRun(t, home, index, "trigram", opts)
	if len(before.Hits) <= 1 || !reflect.DeepEqual(before.Hits, after.Hits) || len(readHitCounts(history, refs)) == 0 {
		t.Fatal("explicit search changed with saturated history")
	}
	// round2.test.ts:119-143: the entry's results compose with the landed clipper.
	fat := after
	fat.Hits = append([]ChatHit{}, after.Hits...)
	for i := range fat.Hits {
		fat.Hits[i].Text = strings.Repeat("z", 1200)
		fat.Hits[i].Title = scanPtr(fat.Hits[i].Text)
		fat.Hits[i].Context = []ChatContextEntry{{Text: fat.Hits[i].Text, IsMatch: true}}
	}
	clipped := ClipChatResultForJson(fat)
	if !clipped.Clipped || ClipChatResultForJson(after).Clipped {
		t.Fatal(clipped)
	}
	for _, h := range clipped.Hits {
		if len(utf16.Encode([]rune(h.Text))) > 501 || len(utf16.Encode([]rune(*h.Title))) > 501 || len(utf16.Encode([]rune(h.Context[0].Text))) > 501 {
			t.Fatal(h)
		}
	}
}

func TestChatSearchRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/chatsearch/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Query string
		Options     ChatSearchOptions
		Out         json.RawMessage
		Error       string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			home, index := chatSearchFixture(t)
			opts := c.Options
			if c.Name != "cold" && c.Name != "empty" && c.Name != "invalid-days" && c.Name != "missing" {
				chatSearchRun(t, home, index, "trigram", ChatSearchOptions{})
			}
			if c.Name == "missing" {
				index = filepath.Join(home, "missing.sqlite")
			}
			if c.Name == "query-failure" {
				chatSearchConfigureIndex(t, index, "DROP TABLE msgs")
			}
			if c.Name == "read-only-open" {
				chatSearchMakeReadOnly(t, index)
			}
			opts.Home, opts.IndexPath = &home, &index
			if c.Name == "invalid-days" {
				opts.Days = scanPtr(math.Inf(1))
			}
			r, err := SearchChat(c.Query, opts, scanTestNow())
			if c.Error != "" {
				if err == nil || err.Error() != c.Error {
					t.Fatalf("error %v want %s", err, c.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r.ElapsedMs = 0
			if r.Index != nil && r.Index.LastIngestAt != nil {
				r.Index.LastIngestAt = scanPtr("<INGEST>")
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			b = []byte(strings.ReplaceAll(string(b), home, "<HOME>"))
			var got, want any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Out, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("oracle differs\ngot %s\nwant %s", b, c.Out)
			}
		})
	}
}
