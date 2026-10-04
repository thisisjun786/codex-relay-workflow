package recall

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type memorySearchOracleFile struct {
	Path, Content string
	Mtime         float64
}

type memorySearchOracleThread struct {
	ID, Cwd string
	Origin  *string
}

type memorySearchOracleCase struct {
	Name, Query, Origin string
	Files               []memorySearchOracleFile
	Threads             []memorySearchOracleThread
	Options             MemorySearchOptions
	MemoryDB, Legacy    bool
	RootFile, HomeFile  bool
	Relative, Error     bool
	Out                 MemorySearchResult
}

func memorySearchOracleHome(t *testing.T, c memorySearchOracleCase) (string, string) {
	t.Helper()
	home, work := filepath.Join(t.TempDir(), "home"), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CRW_HOME", t.TempDir())
	t.Chdir(work)
	if c.HomeFile {
		if err := os.WriteFile(home, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return home, work
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if c.RootFile {
		writeRolloutTestFile(t, home, "memories", "not a directory")
	}
	for _, f := range c.Files {
		p := writeRolloutTestFile(t, home, filepath.Join("memories", f.Path), strings.ReplaceAll(f.Content, "$WORK", work))
		stamp := time.UnixMilli(int64(f.Mtime))
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if c.MemoryDB {
		recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE stage1_outputs (thread_id TEXT PRIMARY KEY, source_updated_at INTEGER NOT NULL, raw_memory TEXT NOT NULL, rollout_summary TEXT NOT NULL)", "")
	}
	if c.Threads != nil {
		schema := "CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', git_branch TEXT, "
		insert := "INSERT INTO threads VALUES (?,?,?,?,?,?)"
		if c.Legacy {
			insert = "INSERT INTO threads VALUES (?,?,?,?,?)"
		} else {
			schema += "git_origin_url TEXT, "
		}
		rows := [][]any{}
		for _, thread := range c.Threads {
			row := []any{thread.ID, "", thread.Cwd, nil}
			if !c.Legacy {
				var origin any
				if thread.Origin != nil {
					origin = *thread.Origin
				}
				row = append(row, origin)
			}
			rows = append(rows, append(row, 0))
		}
		recallFixtureDB(t, home, "state_1.sqlite", schema+"updated_at_ms INTEGER)", insert, rows...)
	}
	return home, work
}

// Generated synthetic inputs and independently recorded CXC answers are rebuilt
// here; neither Node nor a real CODEX_HOME is used by the replay.
func TestMemorySearchOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/memorysearch/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []memorySearchOracleCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 50 {
		t.Fatalf("unexpected corpus size: %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			home, work := memorySearchOracleHome(t, c)
			c.Options.Home = &home
			c.Options.ReadOriginUrl = func(string) string { return c.Origin }
			got, err := SearchMemory(c.Query, c.Options)
			if c.Error {
				if err == nil {
					t.Fatal("oracle throws an IO error; Go returned success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got.ElapsedMs = 0
			for i := range got.Hits {
				if got.Hits[i].Cwd != nil {
					v := strings.ReplaceAll(*got.Hits[i].Cwd, work, "$WORK")
					got.Hits[i].Cwd = &v
				}
			}
			if len(got.Hits) != len(c.Out.Hits) {
				t.Fatalf("hits %d, want %d: %+v", len(got.Hits), len(c.Out.Hits), got)
			}
			for i := range got.Hits {
				memoryFloatEqual(t, got.Hits[i].Score, c.Out.Hits[i].Score)
				got.Hits[i].Score = c.Out.Hits[i].Score
			}
			for i := range got.Warnings {
				got.Warnings[i] = strings.ReplaceAll(strings.ReplaceAll(got.Warnings[i], home, "$HOME"), work, "$WORK")
			}
			if !reflect.DeepEqual(got, c.Out) {
				t.Fatalf("got %+v\nwant %+v", got, c.Out)
			}
		})
	}
}

// memory-search.test.ts:40-46,57-80,119-169, on the existing fixtures.ts port.
func TestMemorySearchSharedFixture(t *testing.T) {
	home := buildRecallCodexHome(t, time.UnixMilli(1_783_382_400_000))
	for _, row := range []struct {
		query, path string
		line        int
	}{
		{"trigram sidecar", "MEMORY.md", 3}, {"wombat migration", "windows-notes.md", 3}, {"한글 검색", "MEMORY.md", 5},
	} {
		r, err := SearchMemory(row.query, MemorySearchOptions{}) // CODEX_HOME defaults to fixture.
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, h := range r.Hits {
			if h.Relpath == row.path && h.StartLine != nil && *h.StartLine == row.line {
				found = true
			}
			if strings.ContainsRune(h.Excerpt, '\r') {
				t.Fatal("CR leaked into excerpt")
			}
		}
		if !found {
			t.Fatalf("%s: expected %s:%d", row.query, row.path, row.line)
		}
	}
	r, err := SearchMemory("한글 없는단어조합", MemorySearchOptions{Home: &home})
	if err != nil || len(r.Hits) != 0 {
		t.Fatalf("AND semantics: %+v, %v", r, err)
	}
}

func TestMemorySearchNonfiniteLimit(t *testing.T) {
	home := t.TempDir()
	writeRolloutTestFile(t, home, "memories/a.md", "zebra")
	writeRolloutTestFile(t, home, "memories/b.md", "zebra")
	for _, limit := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Limit: &limit})
		want := 2
		if math.IsInf(limit, -1) {
			want = 1
		}
		if err != nil || len(r.Hits) != want {
			t.Fatalf("limit %v: %+v, %v", limit, r, err)
		}
	}
}

func TestMemorySearchScopeAndReadErrors(t *testing.T) {
	home := t.TempDir()
	path := writeRolloutTestFile(t, home, "memories/a.md", "thread_id: kept\ncwd: /proj/here\n\nPR3956")
	calls := 0
	opts := MemorySearchOptions{Home: &home, Cwd: memoryPtr("/proj/here"), ReadOriginUrl: func(string) string { calls++; return "" }}
	r, err := SearchMemory("3956", opts)
	if err != nil || len(r.Hits) != 1 || calls != 1 {
		t.Fatalf("retry must reuse scope/origin: %+v, %v, calls %d", r, err, calls)
	}
	opts.ReadOriginUrl = func(string) string {
		calls++
		// Scope is built after listing, so rename the owned synthetic file to
		// exercise readFileSync's caught error without a timing race.
		if err := os.Rename(path, path+".gone"); err != nil {
			t.Fatal(err)
		}
		return ""
	}
	r, err = SearchMemory("zebra", opts)
	if err != nil || r.ScannedFiles != 0 || !strings.Contains(strings.Join(r.Warnings, "\n"), "unreadable memory file: "+path) {
		t.Fatalf("read error: %+v, %v", r, err)
	}
	warnings := []string{}
	_, err = memorySearchBuildCwdScope(path+".gone", opts, &warnings)
	if err == nil {
		t.Fatal("metadata path enumeration of a file must return an error")
	}
	before := calls
	r, err = SearchMemory(" \t\ufeff", opts)
	if err != nil || calls != before || !reflect.DeepEqual(r.Warnings, []string{"empty query"}) {
		t.Fatalf("empty query must skip filesystem enrichment: %+v, %v", r, err)
	}
}

func TestMemorySearchRetainedThreadSeam(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "memories")
	f := writeRolloutTestFile(t, home, "memories/a.md", "thread_id: t-one\ncwd: /proj/other\n\nHandbook\n\nconsolidated")
	groups := ExpandQueryWords([]string{"Handbook", "consolidated"})
	s := memorySearchState{root: root, files: []string{f}, words: []string{"Handbook", "consolidated"}, groups: groups, present: make([]bool, len(groups)), scope: &CwdScope{prefix: "/proj/here", lowerPrefixes: [2]string{"/proj/here", "\\proj\\here"}, only: true}, warnings: []string{}}
	hits, ids, err := s.memorySearchCollectFiles(groups, true)
	if err != nil || len(hits) != 0 || len(ids) != 0 {
		t.Fatalf("rejected file-span cannot suppress future stage1: %+v, %v, %v", hits, ids, err)
	}
	s.scope.only = false
	hits, ids, err = s.memorySearchCollectFiles(groups, true)
	if err != nil || len(hits) != 1 || !ids["t-one"] {
		t.Fatalf("retained file-span must mark its thread: %+v, %v, %v", hits, ids, err)
	}
	if !reflect.DeepEqual(s.present, []bool{true, true}) {
		t.Fatalf("presence must precede scope filtering: %v", s.present)
	}
}

func TestMemorySearchFractionalMtime(t *testing.T) {
	home := t.TempDir()
	for _, row := range []struct {
		name string
		ns   int64
	}{{"above.md", 750_000}, {"below.md", 250_000}} {
		p := writeRolloutTestFile(t, home, "memories/"+row.name, "zebra")
		if err := os.Chtimes(p, time.Unix(0, row.ns), time.Unix(0, row.ns)); err != nil {
			t.Fatal(err)
		}
	}
	// CXC statSync keeps fractional milliseconds for the cutoff comparison,
	// while Date.toISOString clips them. A UnixMilli-only reader drops both.
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Days: memoryPtr(1.0), NowMs: memoryPtr(86_400_000.5)})
	if err != nil || len(r.Hits) != 1 || r.ScannedFiles != 1 || r.Hits[0].Relpath != "above.md" || r.Hits[0].UpdatedAt == nil || *r.Hits[0].UpdatedAt != "1970-01-01T00:00:00.000Z" {
		t.Fatalf("fractional mtime/ISO clipping: %+v, %v", r, err)
	}
}
