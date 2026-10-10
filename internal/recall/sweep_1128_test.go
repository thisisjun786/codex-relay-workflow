package recall

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// CRW-1128: the recall memory search, stage1, trim, ranking and memory status sweep. One test per item of the issue (the
// known-defects.md line is named on each).

func sweepJobsHome(t *testing.T, finishedAt any) string {
	t.Helper()
	home := t.TempDir()
	recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE jobs (kind TEXT, job_key TEXT, status TEXT, retry_remaining INTEGER, last_error TEXT, finished_at)", "INSERT INTO jobs VALUES (?, ?, ?, ?, ?, ?)",
		[]any{"extract", "a", "done", 3, nil, finishedAt}, []any{"extract", "b", "error", 0, "rate limit", nil})
	return home
}

// :537 -- a finished_at that is no number is shown as unreadable, not as NaNd ago, and it is reported.
func TestSweep1128NonNumericTimeIsReportedUnreadable(t *testing.T) {
	home := sweepJobsHome(t, "not a time")
	s := CollectMemoryStatus(home)
	if s.State != MemoryStatusOK || s.LastSuccessAt != nil {
		t.Fatalf("state %s, last success %v", s.State, s.LastSuccessAt)
	}
	text := FormatMemoryStatus(s, 1_000_000)
	if strings.Contains(text, "NaN") || !strings.Contains(text, "last success: unreadable") {
		t.Fatalf("%s", text)
	}
	notice := MemoryStatusNotice(s, 1_000_000)
	if !strings.Contains(notice, "unreadable") || strings.Contains(notice, "NaN") {
		t.Fatalf("a time that cannot be read is no silence: %q", notice)
	}
	// A readable time is unchanged.
	if s = CollectMemoryStatus(sweepJobsHome(t, 900_000.0)); s.LastSuccessAt == nil || *s.LastSuccessAt != 900_000 || strings.Contains(FormatMemoryStatus(s, 1_000_000), "unreadable") {
		t.Fatalf("%+v", s)
	}
}

// :538 -- a store that cannot be read is told apart from a store of another schema, and the counts read before the failure stay.
func TestSweep1128UnreadableStoreIsNotAnUnsupportedSchema(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "memories_1.sqlite"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := CollectMemoryStatus(home)
	if s.State == MemoryStatusUnsupported {
		t.Fatalf("a corrupt store is no other schema: %+v", s)
	}
	if s.State != MemoryStatusUnavailable || !strings.Contains(s.Detail, "could not be read") {
		t.Fatalf("%+v", s)
	}
	if n := MemoryStatusNotice(s); strings.Contains(n, "unsupported store schema") || !strings.Contains(n, "could not be read") {
		t.Fatalf("notice %q", n)
	}
	// A real schema difference stays unsupported.
	other := t.TempDir()
	recallFixtureDB(t, other, "memories_1.sqlite", "CREATE TABLE jobs (kind TEXT, status TEXT)", "")
	if s = CollectMemoryStatus(other); s.State != MemoryStatusUnsupported {
		t.Fatalf("%+v", s)
	}
	// An integer too large for a JavaScript number fails the read of the time, not the schema; the job counts read before it stay.
	big := sweepJobsHome(t, int64(1)<<62)
	s = CollectMemoryStatus(big)
	if s.State == MemoryStatusUnsupported {
		t.Fatalf("an unsafe integer is no schema difference: %+v", s)
	}
	if len(s.Jobs) == 0 {
		t.Fatalf("the counts read before the failure are discarded: %+v", s)
	}
}

// :591 -- the thread id is on the line of its key.
func TestSweep1128ThreadIDStaysOnItsLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"thread_id:\n# next", ""}, {"thread_id: \t\ncwd: /x", ""}, {"cwd: /x\nthread_id: real\n", "real"}, {"thread_id:\nthread_id: second", "second"},
		{"thread_id:   spaced  \n", "spaced"}, {"cwd: /x\r\nthread_id: crlf\r\n", "crlf"}, {"xthread_id: no", ""},
	} {
		got := frontmatterThreadID(tc.in)
		if tc.want == "" && got != nil || tc.want != "" && (got == nil || *got != tc.want) {
			t.Errorf("%q: got %v, want %q", tc.in, got, tc.want)
		}
	}
}

// :592 -- a cwd with spaces or quotes is read whole.
func TestSweep1128CwdWithSpacesAndQuotes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"cwd: /a b\n", "/a b"}, {"cwd: \"/a b\"\n", "/a b"}, {"cwd: '/x y/z'\r\n", "/x y/z"}, {"thread_id: t\ncwd:   /plain  \n", "/plain"},
		{"cwd: \"/open\n", "\"/open"}, {"cwd: /with'quote\n", "/with'quote"},
	} {
		got := frontmatterCwd(tc.in)
		if got == nil || *got != tc.want {
			t.Errorf("%q: got %v, want %q", tc.in, got, tc.want)
		}
	}
	if got := frontmatterCwd("cwd:\n"); got != nil {
		t.Errorf("an empty cwd is none: %q", *got)
	}
}

// :593 -- the excerpt is cut where the term is in the original text, even after characters that lowercase to two.
func TestSweep1128ExcerptOffsetIsTheOriginalOffset(t *testing.T) {
	content := strings.Repeat("İ", 300) + "needle" + strings.Repeat("x", 600)
	term := QueryTerm{Text: "needle"}
	got := excerptAround(content, term, 400)
	if want := memorySlice(content, 100, 500); got != want || !strings.Contains(got, "needle") {
		t.Fatalf("excerpt starts at the wrong character: %q", memorySlice(got, 0, 20))
	}
	// Text that lowercases one to one is cut as before.
	plain := strings.Repeat("a", 300) + "needle" + strings.Repeat("x", 600)
	if got, want := excerptAround(plain, term, 400), memorySlice(plain, 100, 500); got != want {
		t.Fatal("plain text moved")
	}
}

func sweepHit(path string, score float64, date string, line int) MemoryHit {
	return MemoryHit{Origin: "file", Kind: MemoryHandbook, Relpath: path, UpdatedAt: &date, StartLine: &line, Score: score}
}

// :694 and :695 -- equal hits compare as equal and a stable identity breaks the tie; NaN scores rank last.
func TestSweep1128RankingIsAConsistentOrder(t *testing.T) {
	a, b, c := sweepHit("a.md", 1, "2026-01-01", 1), sweepHit("b.md", 1, "2026-01-01", 1), sweepHit("c.md", 1, "2026-01-01", 1)
	var want []string
	for _, order := range [][]MemoryHit{{a, b, c}, {c, b, a}, {b, c, a}, {b, a, c}, {a, c, b}, {c, a, b}} {
		got := []string{}
		for _, h := range RankAndTrim(slices.Clone(order), 10) {
			got = append(got, h.Relpath)
		}
		if want == nil {
			want = got
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the order depends on the input order: %v then %v", want, got)
		}
	}
	if !slices.Equal(want, []string{"a.md", "b.md", "c.md"}) {
		t.Fatalf("identity tie break: %v", want)
	}
	nan := sweepHit("nan.md", math.NaN(), "2026-02-01", 1)
	for _, order := range [][]MemoryHit{{nan, a, b}, {a, nan, b}, {b, a, nan}} {
		got := RankAndTrim(slices.Clone(order), 10)
		if len(got) != 3 || got[0].Relpath != "a.md" || got[1].Relpath != "b.md" || got[2].Relpath != "nan.md" {
			t.Fatalf("a NaN score does not rank last: %+v", got)
		}
	}
	// A newer date wins over identity, a better score over a newer date.
	got := RankAndTrim([]MemoryHit{sweepHit("a.md", 1, "2026-01-01", 1), sweepHit("z.md", 1, "2026-03-01", 1), sweepHit("m.md", 2, "2025-01-01", 1)}, 10)
	if got[0].Relpath != "m.md" || got[1].Relpath != "z.md" || got[2].Relpath != "a.md" {
		t.Fatalf("%+v", got)
	}
}

// :696 -- a limit of zero returns nothing; a fraction is whole hits.
func TestSweep1128TrimLimitIsNormalized(t *testing.T) {
	hits := []MemoryHit{sweepHit("a.md", 3, "2026-01-01", 1), sweepHit("b.md", 2, "2026-01-01", 1), sweepHit("c.md", 1, "2026-01-01", 1)}
	for limit, want := range map[float64]int{0: 0, -1: 0, 0.5: 0, 1: 1, 1.9: 1, 2: 2, 100: 3, math.Inf(1): 3, math.Inf(-1): 0} {
		if got := RankAndTrim(slices.Clone(hits), limit); len(got) != want {
			t.Errorf("limit %v: %d hits, want %d", limit, len(got), want)
		}
	}
}

func sweepIndexDB(t *testing.T) (*RwDb, string) {
	t.Helper()
	home := sweepIndexHome(t, []string{"/proj/alpha", "/proj/alpha", "/proj/alpha"}, []string{"deploy one", "deploy two", "deploy three"})
	db, _ := indexTestDB(t)
	if _, err := ingest(home, db, 0); err != nil {
		t.Fatal(err)
	}
	return db, home
}

func sweepIndexQuery(home string, limit float64, order ChatOrder, now *float64) IndexQueryOptions {
	return IndexQueryOptions{Plan: ChatMatchPlan("deploy", true, false), Limit: limit, Source: RolloutAll, IncludeTools: true, Home: home, Order: order, NowMs: now}
}

// :737 -- a clock that is no number is refused before anything is ranked.
func TestSweep1128NonFiniteClockIsRefused(t *testing.T) {
	db, home := sweepIndexDB(t)
	for _, now := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := queryIndex(db, sweepIndexQuery(home, 5, ChatRelevance, &now)); err == nil || !strings.Contains(err.Error(), "nowMs") {
			t.Errorf("index ranking at %v: %v", now, err)
		}
		if _, err := SearchChat("deploy", ChatSearchOptions{Home: &home, NowMs: &now, Days: scanPtr(0.0)}, scanTestNow()); err == nil || !strings.Contains(err.Error(), "nowMs") {
			t.Errorf("chat search at %v: %v", now, err)
		}
		if _, err := SearchMemory("deploy", MemorySearchOptions{Home: &home, NowMs: &now}); err == nil || !strings.Contains(err.Error(), "nowMs") {
			t.Errorf("memory search at %v: %v", now, err)
		}
	}
}

// :738 -- a fractional limit is whole rows before the pool is sized.
func TestSweep1128FractionalIndexLimit(t *testing.T) {
	db, home := sweepIndexDB(t)
	for _, order := range []ChatOrder{ChatRelevance, ChatRecent} {
		for limit, want := range map[float64]int{1.5: 1, 2.25: 2, 10.25: 3, 0.4: 0} {
			r, err := queryIndex(db, sweepIndexQuery(home, limit, order, nil))
			if err != nil || len(r.Hits) != want {
				t.Errorf("order %q limit %v: %d hits, want %d: %v", order, limit, len(r.Hits), want, err)
			}
		}
	}
	for _, limit := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := queryIndex(db, sweepIndexQuery(home, limit, ChatRelevance, nil)); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Errorf("limit %v: %v", limit, err)
		}
	}
}

func sweepMemoryHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for name, content := range files {
		writeRolloutTestFile(t, home, "memories/"+name, content)
	}
	return home
}

// :762 -- a path mentioned in prose counts only as a whole path.
func TestSweep1128ProseCwdIsAWholePath(t *testing.T) {
	home := sweepMemoryHome(t, map[string]string{
		"adjacent.md": "notes about /proj/here-adjacent and more zebra\n",
		"longer.md":   "zebra lives in /proj/here2/deep\n",
		"parent.md":   "zebra under /x/proj/here only\n",
		"whole.md":    "zebra in /proj/here.\n",
		"child.md":    "zebra in (/proj/here/sub/file.go) today\n",
		"quoted.md":   "zebra in \"/proj/here\"\n",
		"end.md":      "zebra in /proj/here",
	})
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Cwd: memoryPtr("/proj/here"), CwdOnly: true, ReadOriginUrl: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.Relpath)
	}
	slices.Sort(got)
	if want := []string{"child.md", "end.md", "quoted.md", "whole.md"}; !slices.Equal(got, want) {
		t.Fatalf("memories inside /proj/here: %v, want %v", got, want)
	}
}

// :763 -- an age cutoff of exactly zero still filters.
func TestSweep1128ZeroCutoffStillFilters(t *testing.T) {
	home := sweepMemoryHome(t, map[string]string{"old.md": "zebra old\n"})
	old := time.Unix(-1000, 0)
	if err := os.Chtimes(filepath.Join(home, "memories", "old.md"), old, old); err != nil {
		t.Fatal(err)
	}
	recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE stage1_outputs (thread_id TEXT PRIMARY KEY, source_updated_at INTEGER NOT NULL, raw_memory TEXT NOT NULL, rollout_summary TEXT NOT NULL)", "INSERT INTO stage1_outputs VALUES (?, ?, ?, ?)",
		[]any{"t-old", -100, "zebra from before the epoch", "s"}, []any{"t-new", 100, "zebra after the epoch", "s"})
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Days: memoryPtr(1.0), NowMs: memoryPtr(86_400_000.0), Synonyms: memoryPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.Relpath)
	}
	if !slices.Equal(got, []string{"stage1_outputs/t-new"}) {
		t.Fatalf("a cutoff at the epoch keeps what is newer: %v", got)
	}
}

func sweepStage1Home(t *testing.T, rows ...[]any) string {
	t.Helper()
	home := t.TempDir()
	recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE stage1_outputs (thread_id TEXT, source_updated_at INTEGER NOT NULL, raw_memory TEXT NOT NULL, rollout_summary TEXT NOT NULL)", "INSERT INTO stage1_outputs VALUES (?, ?, ?, ?)", rows...)
	return home
}

// :814 -- the stage1 prefilter finds text the final predicate accepts, whatever its letters' case.
func TestSweep1128Stage1PrefilterFoldsUnicode(t *testing.T) {
	now := time.Now().Unix()
	home := sweepStage1Home(t, []any{"t1", now, "Über alles", "s"}, []any{"t2", now, "İstanbul trip", "s"}, []any{"t3", now, "plain row", "s"})
	for query, want := range map[string]string{"über": "stage1_outputs/t1", "ÜBER": "stage1_outputs/t1"} {
		r, err := SearchMemory(query, MemorySearchOptions{Home: &home, Synonyms: memoryPtr(false)})
		if err != nil || len(r.Hits) != 1 || r.Hits[0].Relpath != want {
			t.Errorf("%q: %+v, %v", query, r.Hits, err)
		}
	}
}

// :814 -- a query whose default synonym group has more spellings than the prefilter lists is left to the final predicate, and the
// search reads the store all the same.
func TestSweep1128Stage1ManySpellingsGroupIsLeftToTheFinalPredicate(t *testing.T) {
	now := time.Now().Unix()
	home := sweepStage1Home(t, []any{"t1", now, "verify skill session initial", "s"}, []any{"t2", now, "plain row", "s"})
	for _, query := range []string{"verify", "verification", "initial verification"} {
		r, err := SearchMemory(query, MemorySearchOptions{Home: &home})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range r.Warnings {
			if strings.Contains(w, "unreadable") {
				t.Errorf("%q: the store was reported unreadable: %q", query, w)
			}
		}
		if len(r.Hits) != 1 || r.Hits[0].Relpath != "stage1_outputs/t1" {
			t.Errorf("%q: %+v", query, r.Hits)
		}
	}
	// The spellings of a letter are listed once each.
	if got := lowerSources('i'); !slices.Equal(got, []rune{'I', 0x130}) {
		t.Errorf("sources of i: %q", got)
	}
}

// :814 -- a letter whose lower case is longer than itself (İ lowers to i and a combining dot) is found wherever it stands in the query.
func TestSweep1128Stage1PrefilterFoldsExpandedLowerCase(t *testing.T) {
	now := time.Now().Unix()
	home := sweepStage1Home(t, []any{"t1", now, "İstanbul trip", "s"}, []any{"t2", now, "a day in İstanbul now", "s"}, []any{"t3", now, "xİy marks", "s"}, []any{"t4", now, "plain row", "s"})
	for query, want := range map[string][]string{
		"İstanbul":       {"stage1_outputs/t1", "stage1_outputs/t2"},
		"istanbul":       nil, // an ASCII i is not an İ
		"xİy":            {"stage1_outputs/t3"},
		"x":              {"stage1_outputs/t3"},
		"i\u0307stanbul": {"stage1_outputs/t1", "stage1_outputs/t2"},
	} {
		r, err := SearchMemory(query, MemorySearchOptions{Home: &home, Synonyms: memoryPtr(false)})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range r.Hits {
			if h.Origin == "stage1" {
				got = append(got, h.Relpath)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v (warnings %q)", query, got, want, r.Warnings)
		}
	}
}

// :762 -- a quoted path is read to its closing quote: a quoted path that goes on past the working directory is another directory.
func TestSweep1128QuotedPathWithSpacesIsOneWholePath(t *testing.T) {
	home := sweepMemoryHome(t, map[string]string{
		"adjacent.md": "zebra in \"/proj/here adjacent\"\n",
		"sub.md":      "zebra in \"/proj/here/sub dir/file\" today\n",
		"exact.md":    "zebra in '/proj/here' today\n",
		"open.md":     "zebra in \"/proj/here and then some\n",
	})
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Cwd: memoryPtr("/proj/here"), CwdOnly: true, ReadOriginUrl: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.Relpath)
	}
	slices.Sort(got)
	if want := []string{"exact.md", "open.md", "sub.md"}; !slices.Equal(got, want) {
		t.Fatalf("memories inside /proj/here: %v, want %v", got, want)
	}
	// The same rule serves the stage1 rows.
	now := time.Now().Unix()
	stage := sweepStage1Home(t, []any{"t1", now, "zebra in \"/proj/here adjacent\"", "s"}, []any{"t2", now, "zebra in \"/proj/here\"", "s"})
	r, err = SearchMemory("zebra", MemorySearchOptions{Home: &stage, Synonyms: memoryPtr(false), Cwd: memoryPtr("/proj/here"), CwdOnly: true, ReadOriginUrl: func(string) string { return "" }})
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Relpath != "stage1_outputs/t2" {
		t.Fatalf("%+v %v", r.Hits, err)
	}
}

// :762 -- a combining mark continues a name: /proj/heré (e and U+0301) is another directory than /proj/here, for files as for stage1 rows.
func TestSweep1128CombiningMarkContinuesAPath(t *testing.T) {
	home := sweepMemoryHome(t, map[string]string{
		"adjacent.md": "zebra in /proj/heré\n",
		"before.md":   "zebra in é/proj/here\n",
		"exact.md":    "zebra in /proj/here\n",
	})
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Cwd: memoryPtr("/proj/here"), CwdOnly: true, ReadOriginUrl: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.Relpath)
	}
	if want := []string{"exact.md"}; !slices.Equal(got, want) {
		t.Fatalf("memories inside /proj/here: %v, want %v", got, want)
	}
	now := time.Now().Unix()
	stage := sweepStage1Home(t, []any{"t1", now, "zebra in /proj/heré", "s"}, []any{"t2", now, "zebra in /proj/here", "s"})
	r, err = SearchMemory("zebra", MemorySearchOptions{Home: &stage, Synonyms: memoryPtr(false), Cwd: memoryPtr("/proj/here"), CwdOnly: true, ReadOriginUrl: func(string) string { return "" }})
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Relpath != "stage1_outputs/t2" {
		t.Fatalf("%+v %v", r.Hits, err)
	}
}

// :591 -- the identity keys are read from the leading key lines alone: a thread_id in the body (a code example, a line after the blank
// line or a heading) names no memory and does not hide the stage1 row of that id.
func TestSweep1128BodyKeysAreNoIdentity(t *testing.T) {
	for _, in := range []string{
		"cwd: /proj/here\n\n# sample\n```yaml\nthread_id: unrelated\n```\nzebra",
		"intro\nthread_id: unrelated",
		"# heading\nthread_id: unrelated",
		"cwd: /a\n---\nthread_id: unrelated",
	} {
		if got := frontmatterThreadID(in); got != nil {
			t.Errorf("%q: a body key is the identity %q", in, *got)
		}
	}
	if got := frontmatterThreadID("updated_at: 2026\nrollout_path:\ncwd: /a\nthread_id: real\n\n# body"); got == nil || *got != "real" {
		t.Errorf("a key further down the leading lines is read: %v", got)
	}
	if got := frontmatterCwd("thread_id: t\nrollout_path:\ncwd: /a\n"); got == nil || *got != "/a" {
		t.Errorf("an empty value does not end the leading lines: %v", got)
	}
	home := sweepStage1Home(t, []any{"unrelated", time.Now().Unix(), "zebra real memory", "s"})
	writeRolloutTestFile(t, home, "memories/sample.md", "cwd: /proj/here\n\n# sample\n```yaml\nthread_id: unrelated\n```\nzebra")
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Synonyms: memoryPtr(false)})
	if err != nil || len(r.Hits) != 2 {
		t.Fatalf("the body key hid the stage1 row: %+v %v", r.Hits, err)
	}
}

// :591 -- the leading key lines end at a line terminator of the text, as the identity key was found before: CR, U+2028 and U+2029 end a
// line as LF does, so a memory file written with them keeps its thread_id (and the file/stage1 de-duplication it drives), while a key
// after the first line that is no key line is still body.
func TestSweep1128LeadingKeysEndAtAnyLineTerminator(t *testing.T) {
	for _, sep := range []string{"\r", "\u2028", "\u2029", "\r\n", "\n"} {
		in := "updated_at: 2026-10-10" + sep + "thread_id: t-" + "x" + sep + sep + "needle"
		if got := frontmatterThreadID(in); got == nil || *got != "t-x" {
			t.Errorf("%q: the leading thread_id is %v", in, got)
		}
		if got := frontmatterThreadID("intro" + sep + "thread_id: body"); got != nil {
			t.Errorf("%q: a line after prose is the identity %q", sep, *got)
		}
		if got := frontmatterThreadID("a: 1" + sep + sep + "thread_id: body"); got != nil {
			t.Errorf("%q: a key after a blank line is the identity %q", sep, *got)
		}
	}
	home := sweepStage1Home(t, []any{"t", time.Now().Unix(), "zebra stage one memory", "s"})
	writeRolloutTestFile(t, home, "memories/cr.md", "updated_at: 2026-10-10\rthread_id: t\r\rzebra from the file")
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Synonyms: memoryPtr(false)})
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Origin != "file" {
		t.Fatalf("the CR separated thread_id did not suppress its stage1 row: %+v %v", r.Hits, err)
	}
}

// :762 -- the root directory contains every path, so every text is inside it: a memory without a cwd of its own is kept by --cwd-only /
// whether or not it names a path, and keeps the half boost of a mention (the oracle's includes("") answer).
func TestSweep1128RootCwdScopeKeepsEveryText(t *testing.T) {
	home := sweepMemoryHome(t, map[string]string{
		"path.md":  "zebra in /proj/here\n",
		"plain.md": "zebra and other prose\n",
	})
	search := func(cwd *string, only bool) map[string]float64 {
		r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Cwd: cwd, CwdOnly: only, ReadOriginUrl: func(string) string { return "" }})
		if err != nil {
			t.Fatal(err)
		}
		scores := map[string]float64{}
		for _, h := range r.Hits {
			scores[h.Relpath] = h.Score
		}
		return scores
	}
	base := search(nil, false)
	for _, cwd := range []string{"/", "//", "\\\\"} {
		for _, only := range []bool{true, false} {
			got := search(memoryPtr(cwd), only)
			if len(got) != 2 {
				t.Fatalf("--cwd %q only=%v: %v", cwd, only, got)
			}
			for name, score := range got {
				if score <= base[name] {
					t.Errorf("--cwd %q only=%v: %s scores %v, unscoped %v", cwd, only, name, score, base[name])
				}
			}
		}
	}
}

// :816 -- an empty thread id is no thread id.
func TestSweep1128EmptyThreadIDIsAbsent(t *testing.T) {
	now := time.Now().Unix()
	home := sweepStage1Home(t, []any{"", now, "zebra first row", "s"}, []any{"", now - 1, "zebra second row", "s"}, []any{nil, now - 2, "zebra null row", "s"})
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Synonyms: memoryPtr(false)})
	if err != nil || len(r.Hits) != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, h := range r.Hits {
		if !strings.HasPrefix(h.Relpath, "stage1_outputs/unknown-") || h.ThreadID != nil {
			t.Errorf("an empty id has a label and no thread: %+v", h)
		}
	}
}

// :818 -- chat results are added to the memory hits, the chat warnings and the real tool policy are said.
func TestSweep1128ChatFallbackMergesAndExplains(t *testing.T) {
	now := time.Now().Unix()
	home := sweepStage1Home(t, []any{"t1", now, "zebra memory row", "s"})
	chat := func(include bool) ChatSearchFn {
		return func(q string, o ChatSearchOptions) (ChatSearchResult, error) {
			if o.IncludeTools == nil || *o.IncludeTools != include {
				t.Errorf("the tool policy was not passed on: %v", o.IncludeTools)
			}
			return ChatSearchResult{Hits: []ChatHit{{TS: "2026-10-04T12:00:00.000Z", Role: "user", Text: "zebra in a session", File: "/x/rollout.jsonl"}}, Warnings: []string{"state db not found (metadata enrichment off)"}}, nil
		}
	}
	r, err := SearchMemory("zebra", MemorySearchOptions{Home: &home, Synonyms: memoryPtr(false), SearchChat: chat(false), ChatFallbackBelow: memoryPtr(1.0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hits) != 2 || r.Hits[0].Origin != "stage1" || r.Hits[1].Origin != "chat" {
		t.Fatalf("the existing memory hit is dropped: %+v", r.Hits)
	}
	all := strings.Join(r.Warnings, "|")
	if strings.Contains(all, "no memory artifacts matched") || !strings.Contains(all, "state db not found") || !strings.Contains(all, "below the chat fallback threshold") {
		t.Fatalf("warnings %q", all)
	}
	// With nothing in memory the older wording stays, with the real tool policy.
	empty := sweepStage1Home(t, []any{"t9", now, "other row", "s"})
	r, err = SearchMemory("zebra", MemorySearchOptions{Home: &empty, Synonyms: memoryPtr(false), SearchChat: chat(true), ChatIncludeTools: true})
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Origin != "chat" {
		t.Fatalf("%+v %v", r, err)
	}
	all = strings.Join(r.Warnings, "|")
	if !strings.Contains(all, "no memory artifacts matched") || strings.Contains(all, "tool logs excluded") || !strings.Contains(all, "tool logs included") {
		t.Fatalf("warnings %q", all)
	}
}

// :819 -- a database warning is said once, however many passes meet it.
func TestSweep1128DatabaseWarningIsSaidOnce(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "memories_1.sqlite"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := SearchMemory("CI", MemorySearchOptions{Home: &home})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, "memories db unreadable") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d copies of the warning: %q", n, r.Warnings)
	}
}
