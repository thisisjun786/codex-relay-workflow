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

func scanTestNow() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }
func scanPtr[T any](v T) *T  { return &v }
func scanNumber(v *float64, fallback float64) float64 {
	if v != nil {
		return *v
	}
	return fallback
}

// Defaults belong to the future searchChat entry point. Tests drive the private
// scan directly, with the same resolved inputs as the export-only Node oracle.
func scanChat(t *testing.T, home, query string, opts ChatSearchOptions) (ChatSearchResult, error) {
	t.Helper()
	source := RolloutMain
	if opts.Source != nil {
		source = *opts.Source
	}
	repo := ""
	if opts.Cwd != nil && *opts.Cwd != "" {
		repo = repoKeyForCwd(*opts.Cwd, opts.ReadOriginUrl)
	}
	return searchViaScan(query, opts, chatScanShared{home, scanNumber(opts.Days, DefaultDays), math.Min(math.Max(scanNumber(opts.Limit, DefaultLimit), 1), MaxLimit), math.Max(scanNumber(opts.Context, 0), 0), ChatMatchPlan(query, opts.Any, opts.Synonyms), source, repo}, scanTestNow())
}

func mustScanChat(t *testing.T, home, query string, opts ChatSearchOptions) ChatSearchResult {
	t.Helper()
	r, err := scanChat(t, home, query, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestChatScanUpstreamCases(t *testing.T) {
	home := buildRecallCodexHome(t, scanTestNow())
	all := ChatSearchOptions{Days: scanPtr(0.0), Source: scanPtr(RolloutAll)}
	for _, tc := range []struct {
		name, query string
		opts        ChatSearchOptions
		count       int
	}{
		{"archive", "aardwolf", all, 2}, {"archive-pruned", "aardwolf", ChatSearchOptions{}, 0},
		{"and", "trigram korean", all, 2}, {"or", "trigram korean", ChatSearchOptions{Any: true, Source: scanPtr(RolloutAll)}, 3},
		{"main", "trigram", ChatSearchOptions{}, 2}, {"subagent", "trigram", ChatSearchOptions{Source: scanPtr(RolloutSubagent)}, 1},
		{"synthetic-hidden", "zebra", ChatSearchOptions{Role: scanPtr("user")}, 0}, {"synthetic-shown", "zebra", ChatSearchOptions{Role: scanPtr("user"), IncludeSynthetic: true}, 1},
		{"tools", "zebra-in-tool-output", ChatSearchOptions{}, 1}, {"no-tools", "zebra-in-tool-output", ChatSearchOptions{IncludeTools: scanPtr(false)}, 0},
		{"stale-hidden", "ancient question", ChatSearchOptions{}, 0}, {"stale-reached", "ancient question", all, 1},
		{"korean", "트라이그램", ChatSearchOptions{}, 2}, {"context", "한글 트라이그램 결과", ChatSearchOptions{Context: scanPtr(1.0)}, 1},
		{"limit", "trigram", ChatSearchOptions{Source: scanPtr(RolloutAll), Limit: scanPtr(1.0)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mustScanChat(t, home, tc.query, tc.opts)
			if len(r.Hits) != tc.count {
				t.Fatalf("got %d hits, want %d: %+v", len(r.Hits), tc.count, r)
			}
			if r.Mode != "scan" || r.ElapsedMs != 0 {
				t.Fatal(r)
			}
		})
	}
	r := mustScanChat(t, home, "트라이그램", ChatSearchOptions{})
	if len(r.Hits) > 0 && (r.Hits[0].Title == nil || *r.Hits[0].Title != "deploy trigram index" || r.Hits[0].GitBranch == nil || *r.Hits[0].GitBranch != "main") {
		t.Fatal(r)
	}
	r = mustScanChat(t, home, "한글 트라이그램 결과", ChatSearchOptions{Context: scanPtr(1.0), Role: scanPtr("user")})
	if len(r.Hits) != 1 || len(r.Hits[0].Context) != 3 || r.Hits[0].Context[0].Role != "tool" || !r.Hits[0].Context[1].IsMatch || r.Hits[0].Context[2].Role != "assistant" {
		t.Fatal(r)
	}
	for _, q := range []string{"", " "} {
		r := mustScanChat(t, t.TempDir(), q, ChatSearchOptions{Days: scanPtr(math.Inf(1))})
		if !reflect.DeepEqual(r.Warnings, []string{"empty query"}) || r.TotalFiles != 0 {
			t.Fatal(r)
		}
	}
	r = mustScanChat(t, t.TempDir(), "anything", ChatSearchOptions{})
	if len(r.Hits) != 0 || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "state db") {
		t.Fatal(r)
	}
}

func TestChatScanScopeAndMetadata(t *testing.T) {
	home := buildRecallCodexHome(t, scanTestNow())
	for _, tc := range []struct {
		cwd, origin string
		count       int
	}{
		{"/proj/alpha", "", 4}, {"/proj/beta", "", 1}, {"/different/alpha", recallOriginAlpha, 3}, {"/different/beta", recallOriginBeta, 1}, {"/proj/alph", "", 0},
	} {
		t.Run(tc.cwd+tc.origin, func(t *testing.T) {
			calls := 0
			r := mustScanChat(t, home, "trigram", ChatSearchOptions{Days: scanPtr(0.0), Source: scanPtr(RolloutAll), Cwd: scanPtr(tc.cwd), ReadOriginUrl: func(string) string { calls++; return tc.origin }})
			if len(r.Hits) != tc.count || calls != 1 {
				t.Fatalf("hits=%d calls=%d, want %d/1", len(r.Hits), calls, tc.count)
			}
		})
	}
	meta := rolloutTestLine(t, map[string]any{"type": "session_meta", "payload": map[string]any{"id": recallThreadMain}})
	p := filepath.Join("sessions", "2026", "10", "04", "rollout-2026-10-04T99-null.jsonl")
	writeRolloutTestFile(t, home, p, meta+recallFixtureMessage(t, "user", "fallback", ""))
	r := mustScanChat(t, home, "fallback", ChatSearchOptions{})
	if len(r.Hits) != 1 || r.Hits[0].Cwd == nil || *r.Hits[0].Cwd != "/proj/alpha" {
		t.Fatal(r)
	}
	writeRolloutTestFile(t, home, p, strings.Replace(meta, `"id":`, `"cwd":"","id":`, 1)+recallFixtureMessage(t, "user", "fallback", ""))
	r = mustScanChat(t, home, "fallback", ChatSearchOptions{})
	if len(r.Hits) != 1 || r.Hits[0].Cwd == nil || *r.Hits[0].Cwd != "" {
		t.Fatal(r)
	}
}

func TestChatScanOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/chatscan/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Fn, Query, Kind, Error string
		Opts                   ChatSearchOptions
		Files                  map[string]string
		Any, Synonyms          bool
		Entries                []ChatEntry
		Index                  int
		N                      float64
		Out                    json.RawMessage
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 200 {
		t.Fatalf("incomplete recorder: %d cases", len(cases))
	}
	for i, c := range cases {
		t.Run(c.Fn+"/"+c.Query+"/"+c.Kind+"/"+string(rune(i+0x100)), func(t *testing.T) {
			var got any
			var callErr error
			switch c.Fn {
			case "consts":
				got = []int{DefaultDays, DefaultLimit, MaxLimit}
			case "plan":
				got = ChatMatchPlan(c.Query, c.Any, c.Synonyms)
				if !reflect.DeepEqual(got, chatMatchPlan(c.Query, c.Any, c.Synonyms)) {
					t.Fatal("production/test helper drift")
				}
			case "context":
				got, callErr = contextWindow(c.Entries, c.Index, c.N)
			case "scan":
				home := t.TempDir()
				t.Setenv("CODEX_HOME", home)
				if c.Kind != "bare" {
					home = buildRecallCodexHome(t, scanTestNow())
				}
				if c.Kind == "nl" {
					addRecallNlGoldenCorpus(t, home, scanTestNow())
				}
				for name, text := range c.Files {
					writeRolloutTestFile(t, home, name, text)
				}
				if c.Kind == "directory" {
					if err := os.MkdirAll(filepath.Join(home, "sessions/2026/10/04/rollout-2026-10-04T99-directory.jsonl"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if c.Kind == "oversized" {
					p := writeRolloutTestFile(t, home, "sessions/2026/10/04/rollout-2026-10-04T99-large.jsonl", "{\"type\":\"session_meta\",\"payload\":{}}\n")
					if err := os.Truncate(p, 1<<31); err != nil {
						t.Fatal(err)
					}
				}
				switch c.Kind {
				case "nan-limit":
					c.Opts.Limit = scanPtr(math.NaN())
				case "nan-context":
					c.Opts.Context = scanPtr(math.NaN())
				case "nan-days":
					c.Opts.Days = scanPtr(math.NaN())
				case "infinite-days":
					c.Opts.Days = scanPtr(math.Inf(1))
				case "infinite-context":
					c.Opts.Context = scanPtr(math.Inf(1))
				}
				c.Opts.ReadOriginUrl = func(string) string { return recallOriginAlpha }
				r, e := scanChat(t, home, c.Query, c.Opts)
				callErr = e
				for j := range r.Hits {
					r.Hits[j].File = "$R/" + filepath.ToSlash(strings.TrimPrefix(r.Hits[j].File, home+string(filepath.Separator)))
				}
				for j, w := range r.Warnings {
					if strings.HasPrefix(w, "unreadable rollout: ") {
						at := strings.Index(w, " (")
						r.Warnings[j] = strings.ReplaceAll(w[:at], home, "$R") + " (io)"
					}
				}
				got = r
			default:
				t.Fatalf("unknown oracle unit %s", c.Fn)
			}
			if c.Error != "" {
				if callErr == nil || c.Error != "io" && callErr.Error() != c.Error {
					t.Fatalf("error %v, oracle %s", callErr, c.Error)
				}
				return
			}
			if callErr != nil {
				t.Fatal(callErr)
			}
			var want any
			if err := json.Unmarshal(c.Out, &want); err != nil {
				t.Fatal(err)
			}
			if out := canon(t, got); !reflect.DeepEqual(out, want) {
				t.Fatalf("got %v, oracle %v", out, want)
			}
		})
	}
}

func TestChatScanCutoffRangeAndContext(t *testing.T) {
	for _, tc := range []struct {
		days float64
		want string
		fail bool
	}{
		{1, "2026-10-03T12:00:00.000Z", false}, {750000, "-000027-04-30T12:00:00.000Z", false}, {math.Inf(1), "", true},
	} {
		got, err := chatScanCutoff(scanTestNow(), tc.days)
		if (err != nil) != tc.fail || !tc.fail && got != tc.want {
			t.Errorf("days %v: %s %v", tc.days, got, err)
		}
	}
	entries := []ChatEntry{{TS: "a", Text: "one"}, {TS: "b", Text: "two"}}
	for _, tc := range []struct {
		index int
		n     float64
		count int
		fail  bool
	}{{0, .5, 1, false}, {1, 1.5, 2, false}, {1, .5, 0, true}, {1, math.Inf(1), 2, false}, {1, math.NaN(), 0, false}} {
		got, err := contextWindow(entries, tc.index, tc.n)
		if (err != nil) != tc.fail || !tc.fail && len(got) != tc.count {
			t.Errorf("%+v: %v %v", tc, got, err)
		}
	}
}

func TestChatFixtureMemorySubstrate(t *testing.T) {
	home := buildRecallCodexHome(t, scanTestNow())
	addRecallNlGoldenCorpus(t, home, scanTestNow())
	for _, name := range []string{"memories/MEMORY.md", "memories/windows-notes.md", "memories/rollout_summaries/summary-aaa.md", "memories/nl-d3.md", "memories/nl-c5.md", "state_2.sqlite", "state_1.sqlite", "memories_1.sqlite"} {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openDbReadOnly(filepath.Join(home, "memories_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmt, err := db.Prepare("SELECT raw_memory FROM stage1_outputs WHERE thread_id = ?")
	if err != nil {
		t.Fatal(err)
	}
	row, err := stmt.Get(recallThreadSub)
	if err != nil || row["raw_memory"] != "db-only memory row about quagga migrations" {
		t.Fatal(row, err)
	}
}
