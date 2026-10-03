package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func envMap(values map[string]string) host.LookupEnv {
	return func(k string) (string, bool) { v, ok := values[k]; return v, ok }
}
func str(s string) *string { return &s }
func writeCache(t *testing.T, dir, body string, at time.Time) string {
	t.Helper()
	file := filepath.Join(dir, "k.cache")
	if err := os.WriteFile(file, []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, at, at); err != nil {
		t.Fatal(err)
	}
	return file
}
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestCacheDir(t *testing.T) {
	home := t.TempDir()
	for _, v := range []struct{ override, want string }{{"", filepath.Join(home, ".crw", "skill-cache")}, {"custom", filepath.Join("custom", "skill-cache")}, {"  ", filepath.Join(home, ".crw", "skill-cache")}, {" custom ", filepath.Join(" custom ", "skill-cache")}} {
		got, err := CacheDir(envMap(map[string]string{"HOME": home, "CRW_HOME": v.override}))
		if err != nil || got != v.want {
			t.Fatalf("%q: %q %v", v.override, got, err)
		}
	}
}
func TestCacheFresh(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	writeCache(t, dir, "cached", now)
	calls := 0
	got, err := CachedFetchText("k", func() (string, error) { calls++; return "net", nil }, CacheOptions{Dir: dir, Now: func() time.Time { return now }})
	if err != nil || got.Text != "cached" || got.Stale || calls != 0 {
		t.Fatalf("%+v %v calls=%d", got, err, calls)
	}
}
func TestCacheExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	file := writeCache(t, dir, "old", now.Add(-2*time.Hour))
	got, err := CachedFetchText("k", func() (string, error) { return "new", nil }, CacheOptions{Dir: dir, Now: func() time.Time { return now }})
	if err != nil || got.Text != "new" || got.Stale || readFile(t, file) != "new" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestCacheStaleAndMiss(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	writeCache(t, dir, "stale", now.Add(-2*time.Hour))
	failure := errors.New("offline")
	fetch := func() (string, error) { return "", failure }
	var warning bytes.Buffer
	opts := CacheOptions{Dir: dir, Now: func() time.Time { return now }, Warnings: &warning}
	got, err := CachedFetchText("k", fetch, opts)
	if err != nil || got.Text != "stale" || !got.Stale {
		t.Fatalf("%+v %v", got, err)
	}
	if warning.String() != "skill-search: network fetch failed for k; serving stale cache (offline)\n" {
		t.Fatal(warning.String())
	}
	if _, err := CachedFetchText("missing", fetch, opts); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
func TestCacheThresholdRefreshAndZero(t *testing.T) {
	now := time.Unix(2000000000, 0)
	zero := time.Duration(0)
	for _, v := range []struct {
		name    string
		age     time.Duration
		refresh bool
		ttl     *time.Duration
		calls   int
	}{{"within", time.Hour - time.Millisecond, false, nil, 0}, {"exact", time.Hour, false, nil, 1}, {"refresh", 0, true, nil, 1}, {"zero", 0, false, &zero, 1}, {"future", -time.Hour, false, nil, 0}} {
		t.Run(v.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCache(t, dir, "old", now.Add(-v.age))
			calls := 0
			_, err := CachedFetchText("k", func() (string, error) { calls++; return "new", nil }, CacheOptions{Dir: dir, Now: func() time.Time { return now }, Refresh: v.refresh, TTL: v.ttl})
			if err != nil || calls != v.calls {
				t.Fatalf("%v calls=%d", err, calls)
			}
		})
	}
}
func TestCacheWriteFailureKeepsWholeFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	file := writeCache(t, dir, "old", now.Add(-2*time.Hour))
	if err := os.Chmod(file, 0o444); err != nil {
		t.Fatal(err)
	}
	var warning bytes.Buffer
	got, err := CachedFetchText("k", func() (string, error) { return "replacement", nil }, CacheOptions{Dir: dir, Now: func() time.Time { return now }, Warnings: &warning})
	if err != nil || !got.Stale || got.Text != "old" || readFile(t, file) != "old" {
		t.Fatalf("%+v %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temp leftovers %v %v", entries, err)
	}
	if !strings.Contains(warning.String(), "network fetch failed for k") {
		t.Fatal(warning.String())
	}
}
func TestCacheBoundaries(t *testing.T) {
	for _, key := range []string{"../outside", "bad/key", "bad\\key", "$(echo evil)", ".."} {
		calls := 0
		_, err := CachedFetchText(key, func() (string, error) { calls++; return "x", nil }, CacheOptions{Dir: t.TempDir()})
		if err == nil || calls != 0 {
			t.Fatalf("%q: %v calls=%d", key, err, calls)
		}
	}
	dir := t.TempDir()
	_, err := CachedFetchText("k", func() (string, error) { return strings.Repeat("x", MaxBodyBytes+1), nil }, CacheOptions{Dir: dir})
	if err == nil {
		t.Fatal("oversize body accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "k.cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func row(id, description string) SkillRow {
	return SkillRow{ID: id, Source: SourceJaw, Name: "x", Description: description, RawURL: "https://example.invalid/SKILL.md"}
}
func TestTokenize(t *testing.T) {
	if got := Tokenize("PDF  vision,ocr"); !reflect.DeepEqual(got, []string{"pdf", "vision", "ocr"}) {
		t.Fatal(got)
	}
	if len(Tokenize("   ")) != 0 {
		t.Fatal("nonempty blank query")
	}
}
func TestExactIDOutranksDescription(t *testing.T) {
	got := Rank([]SkillRow{row("telegram-send", "send messages"), row("notify", "telegram-send helper wrapper")}, "telegram-send", 10)
	if len(got) != 2 || got[0].ID != "telegram-send" || got[0].Score <= got[1].Score {
		t.Fatal(got)
	}
}
func TestKoreanDescription(t *testing.T) {
	r := row("one-password", "")
	r.DescriptionKo = str("1Password CLI로 비밀번호 조회")
	if ScoreRow(r, Tokenize("비밀번호")) <= 0 {
		t.Fatal("no Korean match")
	}
}
func TestDemotion(t *testing.T) {
	plain := row("tdd-guide", "tdd loop")
	sup := row("tdd", "tdd loop")
	sup.SupersededBy = str("dev-testing")
	claude := row("tdd-claude", "tdd loop")
	claude.Status = str("claude-specific")
	terms := Tokenize("tdd loop")
	p, s, c := ScoreRow(plain, terms), ScoreRow(sup, terms), ScoreRow(claude, terms)
	if s >= p || c >= p || s <= 0 {
		t.Fatalf("%g %g %g", p, s, c)
	}
	both := sup
	both.Status = str("claude-specific")
	if ScoreRow(both, terms) != s {
		t.Fatal("double demotion")
	}
}
func TestRankFilterAndLimit(t *testing.T) {
	got := Rank([]SkillRow{row("a", "alpha search"), row("b", "unrelated"), row("search", "the search skill")}, "search", 1)
	if len(got) != 1 || got[0].ID != "search" {
		t.Fatal(got)
	}
	if len(Rank(nil, "search", 10)) != 0 || len(Rank([]SkillRow{row("x", "")}, "", 10)) != 0 {
		t.Fatal("nonempty result")
	}
}
func equalJSON(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s\nwant %s", raw, want)
	}
}
func TestRecordedRankingAndTokens(t *testing.T) {
	var fix struct {
		Rows  []SkillRow
		Ranks []struct {
			Limit int
			Rows  json.RawMessage
		}
		Tokens []struct {
			Query string
			Terms []string
		}
	}
	if err := json.Unmarshal([]byte(readFile(t, "testdata/ranking.json")), &fix); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(fix.Rows)
	for _, v := range fix.Ranks {
		equalJSON(t, Rank(fix.Rows, "needle", v.Limit), v.Rows)
	}
	for _, v := range fix.Tokens {
		if got := Tokenize(v.Query); !reflect.DeepEqual(got, v.Terms) {
			t.Fatalf("%q: %q want%q", v.Query, got, v.Terms)
		}
	}
	after, _ := json.Marshal(fix.Rows)
	if !bytes.Equal(before, after) {
		t.Fatal("input mutated")
	}
	r := row("same", "same")
	r.Name = "same"
	r.Category = str("same")
	r.DescriptionKo = str("same")
	if got := ScoreRow(r, []string{"same", "same"}); got != 42 {
		t.Fatal(got)
	}
}
func TestRecordedSources(t *testing.T) {
	var fix struct {
		Body     map[string]string
		Expected map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(readFile(t, "testdata/catalogs.json")), &fix); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		name, url string
		run       func(FetchText) ([]SkillRow, error)
	}{{"jaw", JAWRegistryURL, FetchJawRows}, {"hermes", HermesCatalogURL, FetchHermesRows}, {"clawhub", ClawhubAPIBase + "/search?q=browser%20cdp&limit=20", func(f FetchText) ([]SkillRow, error) { return SearchClawhubRows(f, "browser cdp") }}} {
		t.Run(v.name, func(t *testing.T) {
			calls := 0
			got, err := v.run(func(u string) (string, error) {
				calls++
				if u != v.url {
					t.Fatalf("unexpected fetch %q", u)
				}
				return fix.Body[v.name], nil
			})
			if err != nil || calls != 1 {
				t.Fatalf("%v calls=%d", err, calls)
			}
			equalJSON(t, got, fix.Expected[v.name])
		})
	}
}
func TestSourceFailuresAndBounds(t *testing.T) {
	failure := errors.New("offline")
	for _, run := range []func(FetchText) ([]SkillRow, error){FetchJawRows, FetchHermesRows, func(f FetchText) ([]SkillRow, error) { return SearchClawhubRows(f, "x") }} {
		if _, err := run(func(string) (string, error) { return "", failure }); !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if _, err := run(func(string) (string, error) { return strings.Repeat("x", MaxBodyBytes+1), nil }); err == nil {
			t.Fatal("oversize accepted")
		}
		if _, err := run(func(string) (string, error) { return string([]byte{255}), nil }); err == nil {
			t.Fatal("invalid utf8 accepted")
		}
	}
	for _, body := range []string{"{", `{} {}`, `{"skills":42}`, `{"skills":{"x":null}}`} {
		if _, err := FetchJawRows(func(string) (string, error) { return body, nil }); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range []string{`{}`, `{"skills":null}`} {
		got, err := FetchJawRows(func(string) (string, error) { return body, nil })
		if err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	}
	for _, body := range []string{`{}`, `{"results":null}`} {
		got, err := SearchClawhubRows(func(string) (string, error) { return body, nil }, "x")
		if err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	}
}
func TestIntentionallyChangedCatalogBoundaries(t *testing.T) {
	var cases []struct{ ID string }
	if err := json.Unmarshal([]byte(readFile(t, "testdata/boundaries.json")), &cases); err != nil {
		t.Fatal(err)
	}
	bodies := []string{`{"skills":[{"name":"one"}]}`, `{"skills":{"one":{"entry":"../outside/SKILL.md"}}}`, `{"skills":{"$(echo evil)":{}}}`, `{"results":[{"slug":"one","summary":42}]}`, "| [`one`](/x) | Description | `../outside` |"}
	for i, v := range cases {
		t.Run(v.ID, func(t *testing.T) {
			run := FetchJawRows
			if i == 3 {
				run = func(f FetchText) ([]SkillRow, error) { return SearchClawhubRows(f, "x") }
			}
			if i == 4 {
				run = FetchHermesRows
			}
			if _, err := run(func(string) (string, error) { return bodies[i], nil }); err == nil {
				t.Fatal("unsafe body accepted")
			}
		})
	}
}
func TestEncodingAndDuplicateKeyOrder(t *testing.T) {
	query := "a b+&/!'()*한글"
	called := ""
	got, err := SearchClawhubRows(func(u string) (string, error) { called = u; return `{"results":[{"slug":"one"}]}`, nil }, query)
	want := ClawhubAPIBase + "/search?q=a%20b%2B%26%2F!'()*%ED%95%9C%EA%B8%80&limit=20"
	if err != nil || called != want || len(got) != 1 || got[0].Name != "one" || got[0].Description != "" {
		t.Fatalf("%s %+v %v", called, got, err)
	}
	got, err = FetchJawRows(func(string) (string, error) {
		return `{"skills":{"z":{"name":"first"},"2":{},"1":{},"z":{"name":"last"}}}`, nil
	})
	if err != nil || len(got) != 3 || got[0].ID != "1" || got[1].ID != "2" || got[2].Name != "last" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestAdapterAndLazyFooter(t *testing.T) {
	if !strings.HasPrefix(AdapterPreamble, "[crw external skill adapter]\n") || !strings.Contains(AdapterPreamble, "(crw-dev)") {
		t.Fatal(AdapterPreamble)
	}
	values := map[string]string{"HOME": t.TempDir(), "CRW_BIN": "first"}
	env := envMap(values)
	first := SearchFooter(env)
	values["CRW_BIN"] = "second"
	second := SearchFooter(env)
	if !strings.Contains(first, "`first skill show <id>`") || !strings.Contains(second, "`second skill show <id>`") {
		t.Fatalf("%s / %s", first, second)
	}
	values["CRW_BIN"] = ""
	if got := SearchFooter(env); !strings.Contains(got, filepath.Join(values["HOME"], ".local", "share", "crw-runtime", "current", "bin", "crw")) {
		t.Fatal(got)
	}
}

func TestHermesJavaScriptLineSeparators(t *testing.T) {
	var cases []struct {
		Name, Body string
		Rows       json.RawMessage
	}
	if err := json.Unmarshal([]byte(readFile(t, "testdata/hermes-lines.json")), &cases); err != nil {
		t.Fatal(err)
	}
	for _, v := range cases {
		t.Run(v.Name, func(t *testing.T) {
			rows, err := FetchHermesRows(func(string) (string, error) { return v.Body, nil })
			if err != nil {
				t.Fatal(err)
			}
			equalJSON(t, rows, v.Rows)
		})
	}
}
