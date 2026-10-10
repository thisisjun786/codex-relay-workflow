package recall

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type memoryStage1OracleCase struct {
	Name, Query, Schema, Origin, ChatError, ChatWarning string
	Rows                                                [][]any
	Files                                               map[string]string
	Threads                                             [][]any
	Options                                             MemorySearchOptions
	Chat                                                []ChatHit
	Calls                                               []ChatSearchOptions
	Legacy, Corrupt, NoChat                             bool
	Out                                                 MemorySearchResult
}

func memoryStage1Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CRW_HOME", t.TempDir())
	return home
}

func memoryStage1Result(t *testing.T, query string, opts MemorySearchOptions) MemorySearchResult {
	t.Helper()
	r, err := SearchMemory(query, opts)
	if err != nil {
		t.Fatal(err)
	}
	r.ElapsedMs = 0
	return r
}

// The new recorder supplies independent full results, including warnings and
// forwarded chat options. Every database and file is rebuilt under t.TempDir.
func TestMemoryStage1Oracle(t *testing.T) {
	data, err := os.ReadFile("testdata/memorystage1/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []memoryStage1OracleCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 46 {
		t.Fatalf("recorded %d cases, want 46", len(cases))
	}
	for _, c := range cases {
		if c.Name == "scope-presence-blocks-relax" {
			// port: fixed (docs/port-cxc/known-defects/CRW-1087.md): TestMemoryStage1PresenceIsCountedInScope.
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			home := memoryStage1Home(t)
			if err := os.MkdirAll(filepath.Join(home, "memories"), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range c.Files {
				p := writeRolloutTestFile(t, home, "memories/"+name, content)
				stamp := time.UnixMilli(int64(*c.Options.NowMs))
				if err := os.Chtimes(p, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if c.Corrupt {
				writeRolloutTestFile(t, home, "memories_1.sqlite", "not sqlite")
			} else {
				schema := c.Schema
				if schema == "" {
					schema = "CREATE TABLE stage1_outputs(thread_id,raw_memory,rollout_summary,source_updated_at)"
				}
				insert := ""
				if len(c.Rows) > 0 {
					insert = "INSERT INTO stage1_outputs VALUES(?,?,?,?)"
				}
				recallFixtureDB(t, home, "memories_1.sqlite", schema, insert, c.Rows...)
			}
			if c.Threads != nil {
				schema, insert := "CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT,cwd TEXT,git_branch TEXT,", "INSERT INTO threads VALUES(?,?,?,?,?,?)"
				rows := [][]any{}
				for _, thread := range c.Threads {
					row := []any{thread[0], "", thread[1], nil}
					if !c.Legacy {
						row = append(row, thread[2])
					}
					rows = append(rows, append(row, 0))
				}
				if c.Legacy {
					insert = "INSERT INTO threads VALUES(?,?,?,?,?)"
				} else {
					schema += "git_origin_url TEXT,"
				}
				recallFixtureDB(t, home, "state_1.sqlite", schema+"updated_at_ms INTEGER)", insert, rows...)
			}
			c.Options.Home = &home
			c.Options.ReadOriginUrl = func(string) string { return c.Origin }
			calls := []ChatSearchOptions{}
			if !c.NoChat && (c.Chat != nil || c.ChatError != "") {
				c.Options.SearchChat = func(_ string, opts ChatSearchOptions) (ChatSearchResult, error) {
					opts.Home = memoryPtr("$HOME")
					calls = append(calls, opts)
					if c.ChatError != "" {
						return ChatSearchResult{}, errors.New(c.ChatError)
					}
					return ChatSearchResult{Hits: c.Chat, Warnings: []string{c.ChatWarning}}, nil
				}
			}
			got := memoryStage1Result(t, c.Query, c.Options)
			if len(got.Hits) != len(c.Out.Hits) {
				t.Fatalf("hits %d, want %d: %+v", len(got.Hits), len(c.Out.Hits), got)
			}
			for i := range got.Hits {
				memoryFloatEqual(t, got.Hits[i].Score, c.Out.Hits[i].Score)
				got.Hits[i].Score = c.Out.Hits[i].Score
			}
			if !reflect.DeepEqual(got, c.Out) {
				t.Fatalf("got %+v\nwant %+v", got, c.Out)
			}
			if !reflect.DeepEqual(canon(t, calls), canon(t, c.Calls)) {
				t.Fatalf("chat options got %+v, want %+v", calls, c.Calls)
			}
			for _, call := range calls {
				if call.ReadOriginUrl == nil || call.ReadOriginUrl("ignored") != c.Origin {
					t.Fatal("origin reader was not forwarded")
				}
			}
		})
	}
}

// B cases: memory-search.test.ts:48-55,171-223 and chat-fallback.test.ts.
// The existing shared fixture owns the rollout and memories SQLite substrate.
func TestMemoryStage1SharedFixture(t *testing.T) {
	memoryStage1Home(t)
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	home := buildRecallCodexHome(t, now)
	opts := MemorySearchOptions{Home: &home, NowMs: memoryPtr(float64(now.UnixMilli()))}
	r := memoryStage1Result(t, "trigram", opts)
	for _, h := range r.Hits {
		if h.Origin == "stage1" && h.ThreadID != nil && *h.ThreadID == recallThreadMain {
			t.Fatal("markdown-covered thread duplicated")
		}
	}
	r = memoryStage1Result(t, "quagga", opts)
	if len(r.Hits) != 1 || r.Hits[0].Origin != "stage1" || r.Hits[0].ThreadID == nil || *r.Hits[0].ThreadID != recallThreadSub {
		t.Fatalf("db-only row missing: %+v", r)
	}
	opts.SearchChat = func(query string, options ChatSearchOptions) (ChatSearchResult, error) {
		return SearchChat(query, options, now)
	}
	r = memoryStage1Result(t, "aardwolf", opts)
	if len(r.Hits) == 0 || r.Hits[0].Origin != "chat" || !strings.Contains(FormatMemoryResult(r), "(chat/chat)") {
		t.Fatalf("archived chat fallback/format missing: %+v", r)
	}
	r = memoryStage1Result(t, "zebra-in-tool-output", opts)
	if len(r.Hits) != 0 {
		t.Fatalf("tool logs leaked: %+v", r)
	}
	opts.SearchChat = nil // cli.ts:213 implements --no-chat by omitting the engine.
	if r := memoryStage1Result(t, "aardwolf", opts); len(r.Hits) != 0 {
		t.Fatalf("nil engine must retain memory-only result: %+v", r)
	}
}

func TestMemoryStage1NonfiniteFallback(t *testing.T) {
	home := memoryStage1Home(t)
	writeRolloutTestFile(t, home, "memories/MEMORY.md", "pangolin")
	for _, threshold := range []float64{-1, math.NaN(), math.Inf(1)} {
		calls := 0
		opts := MemorySearchOptions{Home: &home, ChatFallbackBelow: &threshold, SearchChat: func(string, ChatSearchOptions) (ChatSearchResult, error) {
			calls++
			return ChatSearchResult{Hits: []ChatHit{{TS: "invalid", Text: "chat answer", File: "session.jsonl"}}}, nil
		}}
		r := memoryStage1Result(t, "pangolin", opts)
		want := 1
		if threshold == -1 {
			want = 0
		}
		if calls != want || len(r.Hits) != 1 || want == 1 && r.Hits[0].Origin != "chat" {
			t.Fatalf("threshold %v: calls %d, result %+v", threshold, calls, r)
		}
	}
	limit := math.NaN()
	r := memoryStage1Result(t, "quagga", MemorySearchOptions{Home: &home, Limit: &limit, SearchChat: func(_ string, opts ChatSearchOptions) (ChatSearchResult, error) {
		if opts.Limit == nil || !math.IsNaN(*opts.Limit) {
			t.Fatal("NaN limit not forwarded")
		}
		return ChatSearchResult{Hits: []ChatHit{{Text: "unused"}}}, nil
	}})
	if len(r.Hits) != 0 {
		t.Fatal("Array.slice(0, NaN) must backfill zero hits")
	}
}

// CRW-1087 (A8-03): a boundary term that only an out-of-scope row holds is not present for the scoped search,
// so the relaxed (substring) pass runs; a term an in-scope row holds still blocks it.
func TestMemoryStage1PresenceIsCountedInScope(t *testing.T) {
	now := float64(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC).UnixMilli())
	run := func(t *testing.T, rows [][]any, opts MemorySearchOptions) MemorySearchResult {
		t.Helper()
		home := memoryStage1Home(t)
		recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE stage1_outputs(thread_id,raw_memory,rollout_summary,source_updated_at)", "INSERT INTO stage1_outputs VALUES(?,?,?,?)", rows...)
		recallFixtureDB(t, home, "state_1.sqlite", "CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT,cwd TEXT,git_branch TEXT,updated_at_ms INTEGER)", "INSERT INTO threads VALUES(?,?,?,?,?)",
			[]any{"here", "", "/proj/here", nil, 0}, []any{"here2", "", "/proj/here", nil, 0}, []any{"other", "", "/proj/other", nil, 0})
		opts.Home, opts.NowMs, opts.Cwd, opts.CwdOnly = &home, &now, memoryPtr("/proj/here"), true
		return memoryStage1Result(t, "3956 LSP", opts)
	}
	sec := now / 1000
	t.Run("out of scope", func(t *testing.T) {
		got := run(t, [][]any{{"here", "LSP PR3956", "", sec}, {"other", "3956", "", sec}}, MemorySearchOptions{})
		if len(got.Hits) != 1 || got.Hits[0].Relpath != "stage1_outputs/here" || !strings.Contains(strings.Join(got.Warnings, "|"), "no word-boundary matches") {
			t.Fatalf("the out-of-scope row blocked the relaxed pass: %+v", got)
		}
	})
	t.Run("in scope", func(t *testing.T) {
		got := run(t, [][]any{{"here", "LSP PR3956", "", sec}, {"here2", "3956", "", sec}}, MemorySearchOptions{})
		if len(got.Hits) != 0 {
			t.Fatalf("an in-scope row holds the term and must block the relaxed pass: %+v", got)
		}
	})
}
