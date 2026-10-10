package recall

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRW-1125: the recall query words, chat scan, query conditions and flags sweep. One test per item of the issue (the known-defects.md
// line is named on each).

// :122 -- the 하-verbalizer tail is anchored at the end of the word and keeps a non-empty stem.
func TestSweep1125HaVerbTailKeepsAStem(t *testing.T) {
	for in, want := range map[string]string{"해결했지": "해결", "해결하다": "해결", "해결하고": "해결", "결정했지": "결정", "결정하하하하": "결정"} {
		stemIs(t, in, want)
	}
	for _, in := range []string{"오하해하", "하하하하하", "이해", "하다", "해결", "해하다"} {
		stemIs(t, in, "")
	}
	if got := texts("해결했지"); !contains(got, "해결") {
		t.Fatalf("the stem is not in the group: %q", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// :123 -- an English word spelled with a to f is no commit SHA; a literal SHA still is.
func TestSweep1125HexWordIsNoShaWithoutADigit(t *testing.T) {
	for _, word := range []string{"defaced", "decaffe", "deadbeef"} {
		if IsSymbolWord(word) {
			t.Errorf("%q is gated as a SHA", word)
		}
	}
	for _, word := range []string{"abc1234", "3f2a9c1", "0123456789abcdef0123456789abcdef01234567", "abcdefabcdef", "ABC1234"} {
		if !IsSymbolWord(word) {
			t.Errorf("%q is a SHA", word)
		}
	}
	g := ExpandQueryWords([]string{"defaced"})[0]
	if g[0].Boundary || IsRequiredTerm("defaced") {
		t.Fatalf("defaced is boundary-gated or required: %+v", g)
	}
	// The literal still finds itself.
	plan := ChatMatchPlan("defaced", false, false)
	if !PlanMatches("the page was defaced today", plan) {
		t.Fatal("the word does not match its own text")
	}
}

// sweepChatRollout writes one rollout holding the messages (role, text, hour of 2026-10-04) into a fresh Codex home and returns the home.
func sweepChatRollout(t *testing.T, home, id string, messages [][3]string) string {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	doc := recallFixtureMeta(t, id, "/proj/alpha", "2026-10-04T00:00:00.000Z", "", false)
	for _, m := range messages {
		doc += recallFixtureMessage(t, m[0], m[1], "2026-10-04T"+m[2]+":00:00.000Z")
	}
	writeRolloutTestFile(t, home, filepath.Join("sessions", "2026", "10", "04", "rollout-2026-10-04T01-00-00-"+id+".jsonl"), doc)
	return home
}

func sweepScan(t *testing.T, home, query string, opts ChatSearchOptions) (ChatSearchResult, error) {
	t.Helper()
	opts.Home, opts.Scan = &home, true
	return SearchChat(query, opts, scanTestNow())
}

func sweepTruncated(r ChatSearchResult) bool {
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, "truncated at limit") {
			return true
		}
	}
	return false
}

// :574 -- the limit keeps the newest matches by timestamp, whatever order the rollout lists them in.
func TestSweep1125ScanLimitKeepsTheNewest(t *testing.T) {
	home := sweepChatRollout(t, "", "aa", [][3]string{{"user", "deploy one", "08"}, {"user", "deploy two", "09"}, {"user", "deploy three", "11"}, {"user", "deploy four", "10"}})
	// A second, older rollout of the same day holds an older and a newer match; files are listed newest first.
	sweepChatRollout(t, home, "bb", [][3]string{{"user", "deploy five", "12"}, {"user", "deploy six", "07"}})
	r, err := sweepScan(t, home, "deploy", ChatSearchOptions{Limit: scanPtr(2.0), Days: scanPtr(0.0)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.Text)
	}
	if strings.Join(got, ",") != "deploy five,deploy three" {
		t.Fatalf("the limit kept %q, want the two newest matches", got)
	}
	if !sweepTruncated(r) || r.MatchedFiles != 2 {
		t.Fatalf("warnings %q matched files %d", r.Warnings, r.MatchedFiles)
	}
}

// :575 -- truncated is reported only after another eligible match was seen.
func TestSweep1125TruncatedNeedsAnotherEligibleMatch(t *testing.T) {
	home := sweepChatRollout(t, "", "aa", [][3]string{{"user", "deploy one", "08"}, {"user", "deploy two", "09"}, {"assistant", "unrelated words", "10"}, {"user", "# AGENTS.md instructions injected deploy", "11"}})
	sweepChatRollout(t, home, "bb", [][3]string{{"user", "nothing to see", "06"}})
	r, err := sweepScan(t, home, "deploy", ChatSearchOptions{Limit: scanPtr(2.0), Days: scanPtr(0.0)})
	if err != nil || len(r.Hits) != 2 || sweepTruncated(r) {
		t.Fatalf("two matches under limit 2 are not truncated: %v %q hits %d", err, r.Warnings, len(r.Hits))
	}
	// A role filter or a synthetic message that fails its filter is no extra match.
	r, err = sweepScan(t, home, "deploy", ChatSearchOptions{Limit: scanPtr(1.0), Days: scanPtr(0.0), Role: scanPtr("assistant")})
	if err != nil || len(r.Hits) != 0 || sweepTruncated(r) {
		t.Fatalf("%v %q", err, r.Warnings)
	}
	r, err = sweepScan(t, home, "deploy", ChatSearchOptions{Limit: scanPtr(1.0), Days: scanPtr(0.0)})
	if err != nil || len(r.Hits) != 1 || !sweepTruncated(r) {
		t.Fatalf("a second match is truncation: %v %q", err, r.Warnings)
	}
	// The limit is reached on the very last entry of the last file, and nothing follows: no warning, as for any other entry.
	home = sweepChatRollout(t, "", "cc", [][3]string{{"user", "deploy one", "08"}, {"user", "deploy two", "09"}})
	r, err = sweepScan(t, home, "deploy", ChatSearchOptions{Limit: scanPtr(2.0), Days: scanPtr(0.0)})
	if err != nil || len(r.Hits) != 2 || sweepTruncated(r) {
		t.Fatalf("%v %q", err, r.Warnings)
	}
}

// :576 -- a fractional context window is whole entries, an infinite one is capped, NaN is refused.
func TestSweep1125ContextIsNormalized(t *testing.T) {
	home := sweepChatRollout(t, "", "aa", [][3]string{{"user", "alpha", "08"}, {"assistant", "beta", "09"}, {"user", "deploy gamma", "10"}, {"assistant", "delta", "11"}, {"user", "epsilon", "12"}})
	for _, tc := range []struct {
		context float64
		want    int
	}{{1.5, 3}, {2.9, 5}, {0.5, 1}, {math.Inf(1), 5}, {-1, 1}} {
		for _, scan := range []bool{true, false} {
			index := filepath.Join(t.TempDir(), "index.sqlite")
			opts := ChatSearchOptions{Context: scanPtr(tc.context), Days: scanPtr(0.0), Home: &home, Scan: scan, IndexPath: &index}
			r, err := SearchChat("deploy", opts, scanTestNow())
			if err != nil || len(r.Hits) != 1 {
				t.Fatalf("context %v scan=%v: %v %+v", tc.context, scan, err, r)
			}
			if got := max(len(r.Hits[0].Context), 1); got != tc.want {
				t.Errorf("context %v scan=%v: %d entries, want %d", tc.context, scan, got, tc.want)
			}
		}
	}
	if _, err := sweepScan(t, home, "deploy", ChatSearchOptions{Context: scanPtr(math.NaN())}); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("a NaN context is refused: %v", err)
	}
}

// :577 and :781 -- a days window outside the date range is refused at the entry, for every query and before anything is read; the
// command line says it as a flag error.
func TestSweep1125DaysOutOfRangeIsRefusedAtTheEntry(t *testing.T) {
	home := sweepChatRollout(t, "", "aa", [][3]string{{"user", "deploy", "08"}})
	for _, query := range []string{"deploy", "", "  "} {
		for _, scan := range []bool{true, false} {
			calls := 0
			_, err := SearchChat(query, ChatSearchOptions{Home: &home, Days: scanPtr(1e12), Scan: scan, Cwd: scanPtr("/proj/alpha"), ReadOriginUrl: func(string) string { calls++; return "" }}, scanTestNow())
			if err == nil || !strings.Contains(err.Error(), "--days") || strings.Contains(err.Error(), "Invalid time value") || calls != 0 {
				t.Errorf("query %q scan=%v: %v (origin calls %d)", query, scan, err, calls)
			}
		}
	}
	// The scan refuses the same window when it is called without the entry, and also for an empty query.
	for _, q := range []string{"deploy", ""} {
		if _, err := searchViaScan(q, ChatSearchOptions{}, chatScanShared{Home: home, Days: 1e12, Limit: 5, Plan: ChatMatchPlan(q, false, false), Source: RolloutMain}, scanTestNow()); err == nil || !strings.Contains(err.Error(), "--days") {
			t.Errorf("scan of %q: %v", q, err)
		}
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"chat", "search", "deploy", "--days", "1e12", "--home", home}, &out, &errOut, scanTestNow())
	if code != 1 || !strings.Contains(errOut.String(), "--days") || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, out.String(), errOut.String())
	}
}

// :578 -- a limit that is not a number cannot switch the cap off; a fractional one is whole hits.
func TestSweep1125LimitIsFiniteAndCapped(t *testing.T) {
	var messages [][3]string
	for h := 0; h < 24; h++ {
		messages = append(messages, [3]string{"user", "deploy note " + string(rune('a'+h)), time.Date(2026, 10, 4, h, 0, 0, 0, time.UTC).Format("15")})
	}
	home := sweepChatRollout(t, "", "aa", messages)
	for _, n := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, scan := range []bool{true, false} {
			index := filepath.Join(t.TempDir(), "index.sqlite")
			if _, err := SearchChat("deploy", ChatSearchOptions{Home: &home, Limit: scanPtr(n), Scan: scan, IndexPath: &index, Days: scanPtr(0.0)}, scanTestNow()); err == nil || !strings.Contains(err.Error(), "limit") {
				t.Errorf("limit %v scan=%v is accepted: %v", n, scan, err)
			}
		}
	}
	for _, scan := range []bool{true, false} {
		index := filepath.Join(t.TempDir(), "index.sqlite")
		r, err := SearchChat("deploy", ChatSearchOptions{Home: &home, Limit: scanPtr(2.9), Scan: scan, IndexPath: &index, Days: scanPtr(0.0)}, scanTestNow())
		if err != nil || len(r.Hits) != 2 || !sweepTruncated(r) || !strings.Contains(strings.Join(r.Warnings, "|"), "truncated at limit 2 ") {
			t.Fatalf("scan=%v: limit 2.9 is 2 hits: %v %q %d", scan, err, r.Warnings, len(r.Hits))
		}
	}
}

// :579 -- already fixed by CRW-1123 (known-defects/CRW-1123.md): the file prefilter judges decoded text.
func TestSweep1125EscapedQueryCharactersAreFound(t *testing.T) {
	home := t.TempDir()
	doc := recallFixtureMeta(t, "aa", "/proj/alpha", "2026-10-04T00:00:00.000Z", "", false) +
		`{"timestamp":"2026-10-04T08:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run \u0043I on \"main\""}]}}` + "\n"
	writeRolloutTestFile(t, home, filepath.Join("sessions", "2026", "10", "04", "rollout-2026-10-04T01-00-00-aa.jsonl"), doc)
	for _, q := range []string{"CI", `"main"`} {
		r, err := sweepScan(t, home, q, ChatSearchOptions{})
		if err != nil || len(r.Hits) != 1 {
			t.Errorf("%q: %v %+v", q, err, r)
		}
	}
}

// sweepIndexHome writes one rollout per message text (a distinct hour, cwd as given) and returns the home.
func sweepIndexHome(t *testing.T, cwds []string, texts []string) string {
	t.Helper()
	home := t.TempDir()
	for i, text := range texts {
		id := "id" + string(rune('a'+i))
		doc := recallFixtureMeta(t, id, cwds[i], "2026-10-04T00:00:00.000Z", "", false) + recallFixtureMessage(t, "user", text, "2026-10-04T0"+string(rune('1'+i))+":00:00.000Z")
		writeRolloutTestFile(t, home, filepath.Join("sessions", "2026", "10", "04", "rollout-2026-10-04T0"+string(rune('1'+i))+"-00-00-"+id+".jsonl"), doc)
	}
	return home
}

func sweepSearch(t *testing.T, home, query string, scan bool, opts ChatSearchOptions) ChatSearchResult {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index.sqlite")
	opts.Home, opts.IndexPath, opts.Scan, opts.Days = &home, &index, scan, scanPtr(0.0)
	r, err := SearchChat(query, opts, scanTestNow())
	if err != nil {
		t.Fatalf("%q scan=%v: %v", query, scan, err)
	}
	return r
}

// :661 -- a short word is found in text whose letters SQLite cannot fold (Ü, the Kelvin sign, İ), as the final predicate finds it.
func TestSweep1125ShortWordsAreFoldedLikeTheFinalPredicate(t *testing.T) {
	cwd := []string{"/proj/alpha", "/proj/alpha", "/proj/alpha", "/proj/alpha"}
	home := sweepIndexHome(t, cwd, []string{"Über alles", "temp 300 \u212a here", "\u0130stanbul trip", "plain words only"})
	for _, tc := range []struct {
		word string
		want int
	}{{"ü", 1}, {"k", 1}, {"i", 2}, {"ub", 0}} {
		scan := sweepSearch(t, home, tc.word, true, ChatSearchOptions{Any: true})
		indexed := sweepSearch(t, home, tc.word, false, ChatSearchOptions{Any: true})
		if indexed.Mode != "index" || len(scan.Hits) != len(indexed.Hits) {
			t.Errorf("%q: the index finds %d hits (mode %s), the scan %d", tc.word, len(indexed.Hits), indexed.Mode, len(scan.Hits))
		}
		if tc.want != 0 && len(indexed.Hits) < tc.want {
			t.Errorf("%q: %d hits, want at least %d", tc.word, len(indexed.Hits), tc.want)
		}
	}
}

// :662 -- on a case-insensitive host the index and the scan scope a cwd with the same predicate, non-ASCII letters included.
func TestSweep1125FoldedCwdScopesIndexAndScanAlike(t *testing.T) {
	foldCwdCaseSeam = func() bool { return true }
	t.Cleanup(func() { foldCwdCaseSeam = nil })
	home := sweepIndexHome(t, []string{"/Ü/child", "/ü", "/Ü2", "/other"}, []string{"deploy one", "deploy two", "deploy three", "deploy four"})
	for _, scan := range []bool{true, false} {
		r := sweepSearch(t, home, "deploy", scan, ChatSearchOptions{Cwd: scanPtr("/ü"), ReadOriginUrl: func(string) string { return "" }})
		var got []string
		for _, h := range r.Hits {
			got = append(got, h.Text)
		}
		if len(got) != 2 || !strings.Contains(strings.Join(got, ","), "deploy one") || !strings.Contains(strings.Join(got, ","), "deploy two") {
			t.Errorf("scan=%v: cwd /ü scoped to %q (mode %s), want /Ü/child and /ü", scan, got, r.Mode)
		}
	}
}

// :663 -- a query word with a NUL is not sent to FTS: the index names the reason and the scan answers.
func TestSweep1125NULWordIsServedByTheScan(t *testing.T) {
	home := sweepIndexHome(t, []string{"/proj/alpha"}, []string{"xa\u0000bcx only"})
	r := sweepSearch(t, home, "a\x00bc", false, ChatSearchOptions{})
	if r.Mode != "scan" || len(r.Hits) != 1 {
		t.Fatalf("mode %s hits %d warnings %q", r.Mode, len(r.Hits), r.Warnings)
	}
	if w := strings.Join(r.Warnings, "|"); !strings.Contains(w, "NUL") || strings.Contains(w, "unterminated") {
		t.Fatalf("the reason is not named: %q", w)
	}
}

// :667 -- --help after the -- terminator is a word to search for.
func TestSweep1125HelpStopsAtTheTerminator(t *testing.T) {
	home := sweepIndexHome(t, []string{"/proj/alpha"}, []string{"how to pass --help to a tool"})
	for _, word := range []string{"--help", "-h"} {
		var out, errOut bytes.Buffer
		code := Run([]string{"chat", "search", "--home", home, "--scan", "--days", "0", "--", word}, &out, &errOut, scanTestNow())
		if code != 0 || strings.Contains(out.String(), "crw recall chat search \"<query>\"") || errOut.Len() != 0 {
			t.Errorf("%s: exit %d stdout %q stderr %q", word, code, out.String(), errOut.String())
		}
	}
	if WantsHelp([]string{"chat", "search", "x", "--", "--help"}) || !WantsHelp([]string{"chat", "search", "--help", "--", "x"}) || !WantsHelp([]string{"-h"}) {
		t.Fatal("help is wanted before the terminator only")
	}
}

// :668 -- an explicit home must be a directory.
func TestSweep1125ExplicitHomeMustBeADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExplicitHome(map[string]any{"home": file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a regular file is a home: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"chat", "search", "x", "--home", file}, &out, &errOut, scanTestNow()); code != 1 || !strings.Contains(errOut.String(), "not a directory") {
		t.Fatalf("exit %d stderr %q", code, errOut.String())
	}
}
