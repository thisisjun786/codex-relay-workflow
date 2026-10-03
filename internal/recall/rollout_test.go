package recall

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// CXC recall/test/crlf-recall.test.ts:37-54, with fixed expected entries as well as LF/CRLF equality.
func TestRolloutCRLF(t *testing.T) {
	lf := "{\"type\":\"response_item\",\"timestamp\":\"2026-08-21T00:00:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"first\"}]}}\n" +
		"{\"type\":\"response_item\",\"timestamp\":\"2026-08-21T00:00:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"second\"}]}}\n"
	want := []ChatEntry{
		{TS: "2026-08-21T00:00:00Z", Role: "user", Text: "first", MatchField: "content"},
		{TS: "2026-08-21T00:00:00Z", Role: "user", Text: "second", MatchField: "content"},
	}
	for _, doc := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
		got, err := ParseRollout(doc, true)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("rollout entries = %#v, %v; want %#v", got, err, want)
		}
	}
}

func rolloutUTC(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
}

func rolloutListingHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for _, name := range []string{"sessions/2026/08/21/a.jsonl", "sessions/2026/08/21/z.jsonl", "sessions/2026/08/20/old.jsonl", "sessions/2026/08/21/ignored.txt", "sessions/2026/08/22/new.jsonl", "sessions/2026/99/00/strange.jsonl", "archived_sessions/rollout-2026-08-21T12-00-x.jsonl", "archived_sessions/rollout-2026-08-20T12-00-x.jsonl", "archived_sessions/unparseable.jsonl", "archived_sessions/rollout-2026-99-99T.jsonl"} {
		writeRolloutTestFile(t, home, name, "")
	}
	for _, name := range []string{"directory.jsonl", "😀.jsonl", "\ue000.jsonl"} {
		if err := os.Mkdir(filepath.Join(home, "sessions/2026/08/21", name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "sessions/2026"), filepath.Join(home, "sessions/link")); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestRolloutOracle(t *testing.T) {
	rolloutUTC(t)
	home := rolloutListingHome(t)
	data, err := os.ReadFile(filepath.Join("testdata", "rollout", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		oracleCase
		Error string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i, c := range cases {
		seen[c.Fn]++
		str := func(n int) string { return arg[string](t, c.oracleCase, n) }
		var got any
		var callErr error
		switch c.Fn {
		case "synthetic":
			got = IsSyntheticUserText(str(0))
		case "cwd":
			got = NormalizeCwd(str(0))
		case "cwdMatches":
			got = CwdMatches(str(0), str(1), arg[bool](t, c.oracleCase, 2))
		case "fold":
			got = FoldCwdCaseFor(str(0))
		case "dateName":
			got = DateFromRolloutName(str(0))
		case "sql":
			got = CanonicalCwdSQL(str(0))
		case "localDate":
			d, e := time.Parse(time.RFC3339, str(0))
			if e != nil {
				t.Fatal(e)
			}
			got = LocalDateString(d)
		case "parse":
			got, callErr = ParseRollout(str(0), arg[bool](t, c.oracleCase, 1))
		case "prefilter":
			got = MatchesFilePrefilter(str(0), arg[MatchPlan](t, c.oracleCase, 1))
		case "meta":
			path := filepath.Join(home, "missing")
			if string(c.In[0]) != "null" {
				path = writeRolloutTestFile(t, home, "meta.jsonl", str(0))
			}
			got, callErr = ReadRolloutMeta(path)
		case "list":
			files, e := ListRolloutFiles(home, arg[float64](t, c.oracleCase, 0), time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC))
			callErr = e
			for j := range files {
				files[j].Path = "$R/" + filepath.ToSlash(strings.TrimPrefix(files[j].Path, home+string(filepath.Separator)))
			}
			got = files
		default:
			t.Fatalf("unknown rollout oracle fn %s", c.Fn)
		}
		if c.Error != "" {
			if callErr == nil || c.Error != "io" && callErr.Error() != c.Error {
				t.Errorf("case %d %s: error %v, want %s", i, c.Fn, callErr, c.Error)
			}
			continue
		}
		if callErr != nil {
			t.Fatalf("case %d %s: %v", i, c.Fn, callErr)
		}
		var want any
		if err := json.Unmarshal(c.Out, &want); err != nil {
			t.Fatal(err)
		}
		if g := canon(t, got); !reflect.DeepEqual(g, want) {
			t.Errorf("case %d %s: got %.300v, oracle %.300v", i, c.Fn, g, want)
		}
	}
	if len(cases) != 1146 || len(seen) != 11 {
		t.Fatalf("oracle: %d cases, %d functions", len(cases), len(seen))
	}
	t.Logf("replayed %d recorded cases across %d functions", len(cases), len(seen))
}

func TestRolloutTemporaryHomeFlow(t *testing.T) {
	rolloutUTC(t)
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	home := buildCodexHome(t, now)
	files, err := ListRolloutFiles(home, 30, now)
	if err != nil || len(files) != 3 {
		t.Fatalf("listing = %v, %v", files, err)
	}
	for _, f := range files {
		meta, err := ReadRolloutMeta(f.Path)
		if err != nil || meta.ThreadID == nil || meta.RepoKey == nil || *meta.RepoKey != "example.test/group/repo" {
			t.Fatal(meta, err)
		}
		wantSource := RolloutMain
		if *meta.ThreadID == "sub" {
			wantSource = RolloutSubagent
		}
		if meta.Source != wantSource {
			t.Fatal(meta)
		}
		before, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := ParseRollout(string(before), true)
		if err != nil || len(entries) != 3 || !entries[0].Synthetic || entries[1].Synthetic || entries[2].Role != "assistant" {
			t.Fatal(entries, err)
		}
		after, err := os.ReadFile(f.Path)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("reader changed file", err)
		}
	}
	if got, err := ListRolloutFiles(home, 0, now); err != nil || len(got) != 4 {
		t.Fatal(got, err)
	}
}

func TestRolloutHeadReadAndFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for _, padding := range []int{0, 32768, 131072, 524288} {
		head := `{"type":"session_meta","payload":{"id":"large","instructions":"` + strings.Repeat("x", padding) + `"}}`
		path := writeRolloutTestFile(t, home, "head.jsonl", head+"\r\nsecond\n")
		got, err := readFirstLine(path)
		if err != nil || got != head+"\r" {
			t.Fatal(len(got), err)
		}
		meta, err := ReadRolloutMeta(path)
		if err != nil || meta.ThreadID == nil || *meta.ThreadID != "large" {
			t.Fatal(meta, err)
		}
	}
	path := writeRolloutTestFile(t, home, "head.jsonl", strings.Repeat("x", 1048580))
	if got, err := readFirstLine(path); err != nil || len(got) != 1048576 {
		t.Fatal(len(got), err)
	}
	path = writeRolloutTestFile(t, home, "head.jsonl", "first\rsecond")
	if got, err := readFirstLine(path); err != nil || got != "first\rsecond" {
		t.Fatal(got, err)
	}
	path = writeRolloutTestFile(t, home, "head.jsonl", string([]byte{0xff, '\n'}))
	if got, err := readFirstLine(path); err != nil || got != "\ufffd" {
		t.Fatal(got, err)
	}
	for _, path := range []string{home, filepath.Join(home, "missing")} {
		if _, err := ReadRolloutMeta(path); err == nil {
			t.Fatal("file error swallowed", path)
		}
	}
	if dirs := safeDirs(filepath.Join(home, "missing")); len(dirs) != 0 {
		t.Fatal(dirs)
	}
	if files, err := ListRolloutFiles(filepath.Join(home, "missing"), 0); err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
	writeRolloutTestFile(t, home, "archived_sessions", "")
	if _, err := ListRolloutFiles(home, 0); err == nil {
		t.Fatal("archive listing error swallowed")
	}
}

func TestRolloutLocalMidnightAndInvalidDays(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("west", -7*60*60)
	t.Cleanup(func() { time.Local = old })
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	if got := LocalDateString(now); got != "2026-08-21" {
		t.Fatal(got)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeRolloutTestFile(t, home, "sessions/2026/08/20/keep.jsonl", "")
	writeRolloutTestFile(t, home, "sessions/2026/08/19/drop.jsonl", "")
	files, err := ListRolloutFiles(home, 1, now)
	if err != nil || len(files) != 1 || files[0].Date != "2026-08-20" {
		t.Fatal(files, err)
	}
	for _, c := range []struct {
		days  float64
		count int
	}{{math.NaN(), 2}, {math.Inf(-1), 2}, {math.Inf(1), 0}, {1e20, 0}} {
		if got, err := ListRolloutFiles(home, c.days, now); err != nil || len(got) != c.count {
			t.Fatal(c, got, err)
		}
	}
	if FoldCwdCase() != (runtime.GOOS == "darwin" || runtime.GOOS == "windows") {
		t.Fatal("platform folding")
	}
}

func TestRolloutCwdSQLWithBoundValues(t *testing.T) {
	db, err := openDbReadWrite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmt, err := db.Prepare("SELECT " + CanonicalCwdSQL("cwd") + " AS normalized FROM (SELECT ? AS cwd)")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, want string }{{`\\?\c:\Repo\`, "C:/Repo"}, {"//?/unc/server/share/", "//server/share"}, {"/", ""}, {"/repo/a'b/", "/repo/a'b"}, {"relative//x/", "relative//x"}} {
		got, err := stmt.Get(c.in)
		if err != nil || got["normalized"] != c.want || NormalizeCwd(c.in) != c.want {
			t.Fatal(c, got, err)
		}
	}
}

func TestRolloutKeptParserDefects(t *testing.T) {
	raw := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"\u0043I"}]}}`
	plan := CompileMatchPlan([]QueryGroup{{{Text: "ci"}}}, []string{"ci"}, false, false)
	got, err := ParseRollout(raw, false)
	if err != nil || len(got) != 1 || got[0].Text != "CI" || MatchesFilePrefilter(Lower(raw), plan) || !PlanMatches(Lower(got[0].Text), plan) {
		t.Fatal(got, err)
	}
	if got, err := ParseRollout(strings.Replace(raw, "response_item", `response_\u0069tem`, 1), false); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	if got, err := ParseRollout(raw+" {}", false); err != nil || len(got) != 0 {
		t.Fatal("trailing JSON", got, err)
	}
	bad := `{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":{"toString":null}}]}}`
	if got, err := ParseRollout(raw+"\n"+bad, true); err == nil || got != nil {
		t.Fatal("coercion error must abort without partial results", got, err)
	}
}

// Same platform limits as the predecessor ports: Go strings cannot carry a lone
// UTF-16 surrogate and encoding/json rejects nesting beyond 10,000 levels.
func TestRolloutJSONPlatformBoundaries(t *testing.T) {
	raw := `{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":"\ud800"}]}}`
	got, err := ParseRollout(raw, false)
	if err != nil || len(got) != 1 || got[0].Text != "\ufffd" {
		t.Fatal(got, err)
	}
	deep := `{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":"readable"}]},"extra":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + "}"
	if got, err := ParseRollout(deep, false); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
