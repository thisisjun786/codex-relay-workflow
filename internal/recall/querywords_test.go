package recall

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Ports of the B-class tests of CXC v0.2.40 recall/test/query-words.test.ts whose subject is query-words.ts (the nine that call
// searchMemory or scoreChunk belong to the memory-search port), and pins of where Go and JavaScript strings differ.

func bounded(text string) QueryTerm { return QueryTerm{Text: text, Boundary: true} }
func loose(text string) QueryTerm   { return QueryTerm{Text: text} }

func holds(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

func wantStrings(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	holds(t, slices.Equal(got, want), "%s = %q, want %q", what, got, want)
}

func leads(groups []QueryGroup) (out []string) {
	for _, g := range groups {
		out = append(out, g[0].Text)
	}
	return out
}

// :36, plus the JavaScript white space set: U+FEFF, U+2028 and U+3000 separate words, U+0085, U+180E and U+200B do not.
func TestSplitQueryWords(t *testing.T) {
	wantStrings(t, "raw", SplitQueryWordsRaw("  CI  PR3956 "), "CI", "PR3956")
	wantStrings(t, "lower", SplitQueryWords("  CI  PR3956 "), "ci", "pr3956")
	holds(t, len(SplitQueryWordsRaw("a b c d e f g h i j")) == 10, "MaxWords is a threshold, not a cap")
	many := make([]string, MaxQueryTerms+5)
	for i := range many {
		many[i] = "w" + strconv.Itoa(i)
	}
	holds(t, len(SplitQueryWordsRaw(strings.Join(many, " "))) == MaxQueryTerms, "MaxQueryTerms is the cap")
	wantStrings(t, "separators", SplitQueryWordsRaw("a\u00a0b\u3000c\ufeffd\u2028e\u2003f"), "a", "b", "c", "d", "e", "f")
	for _, w := range []string{"a\u0085b", "a\u180eb", "a\u200bb"} {
		wantStrings(t, "kept whole", SplitQueryWordsRaw(w), w)
	}
	wantStrings(t, "blank", SplitQueryWordsRaw(" \t\n "))
}

// toLowerCase maps U+0130 to two units and a word-final capital sigma to its final form, where unicode.ToLower does neither.
func TestLowerIsToLowerCase(t *testing.T) {
	for in, want := range map[string]string{"CI": "ci", "İSTANBUL": "i\u0307stanbul", "ΑΣ Σ": "ας σ", "\u212a": "k", "Éa": "éa", "\u01c5": "\u01c6", "배포": "배포"} {
		holds(t, Lower(in) == want, "Lower(%q) = %q, want %q", in, Lower(in), want)
	}
}

// :49.
func TestDropStopwords(t *testing.T) {
	wantStrings(t, "CI is the query", DropStopwords([]string{"CI", "문제"}), "CI")
	wantStrings(t, "padding", DropStopwords([]string{"재시작", "문제", "방법"}), "재시작")
	wantStrings(t, "everything would match everything", DropStopwords([]string{"그", "문제"}), "그", "문제")
	wantStrings(t, "not a prefix", DropStopwords([]string{"이해", "문제"}), "이해")
	wantStrings(t, "stopwords", QueryStopwords(), "그", "이", "저", "것", "문제", "방법")
	QueryStopwords()[0] = "x"
	wantStrings(t, "a copy is returned", QueryStopwords()[:1], "그")
}

// :56.
func TestIsRequiredTerm(t *testing.T) {
	for _, w := range []string{"2.49.0", "v2.49.0", "npm", "CI", "3956", "#3956", "hook.ts", "src/hook.ts", "6e97e73d", "Codex", "BundledPluginsMarketplace", "NaiControlsPanel"} {
		holds(t, IsRequiredTerm(w), "%s must be required", w)
	}
	for _, w := range []string{"배포하고", "코덱스를", "deploy", "release", "korean", "지난번", "확인한", "ABCDEFG", "PR3956", "İD"} {
		holds(t, !IsRequiredTerm(w), "%s must be optional", w)
	}
}

// :67 and :81, and the oracle's fallback for a raw word list shorter than the groups: the group's first term judges.
func TestCompileMatchPlan(t *testing.T) {
	raw := SplitQueryWordsRaw("2.49.0 배포하고 npm 패키지가 진짜 그 소스인지 검증한 기록")
	kept := DropStopwords(raw)
	holds(t, len(raw) == 9 && len(kept) == MaxWords, "the ninth word survives, one stopword goes: %d, %d", len(raw), len(kept))
	plan := CompileMatchPlan(ExpandQueryWords(kept), kept, false, len(raw) > MaxWords)
	wantStrings(t, "required", leads(plan.Required), "2.49.0", "npm")
	holds(t, len(plan.Optional) == 6 && plan.MinOptional == 3 && !plan.AnyMode, "got %+v", plan)

	short := []string{"trigram", "korean"}
	strict := CompileMatchPlan(ExpandQueryWords(short), short, false, false)
	holds(t, len(strict.Required) == 2 && len(strict.Optional) == 0 && strict.MinOptional == 0, "under the threshold every group stays required: %+v", strict)
	long := SplitQueryWordsRaw("지난번 로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인한 방법")
	anyPlan := CompileMatchPlan(ExpandQueryWords(long), long, true, true)
	holds(t, anyPlan.AnyMode && len(anyPlan.Required) == 10 && len(anyPlan.Optional) == 0, "--any wins over the quota: %+v", anyPlan)

	missing := CompileMatchPlan(ExpandQueryWords([]string{"CI", "배포", "Codex"}), []string{"CI"}, false, true)
	wantStrings(t, "required", leads(missing.Required), "ci")
	wantStrings(t, "optional", leads(missing.Optional), "배포", "codex")
}

// :95.
func TestPlanMatches(t *testing.T) {
	raw := []string{"2.49.0", "npm", "패키지가", "검증한", "기록"}
	plan := CompileMatchPlan(ExpandQueryWords(raw), raw, false, true)
	holds(t, plan.MinOptional == 2 && len(AllGroups(plan)) == 5 && AllGroups(plan)[0][0].Text == "2.49.0", "ceil(3/2), required first: %+v", plan)
	for text, want := range map[string]bool{
		"2.49.0 npm 패키지가 검증한 기록": true, "2.49.0 npm 패키지가 검증한 내용": true, // two of three optional groups is enough
		"2.49.0 npm 패키지가 다른 내용": false, "배포 npm 패키지가 검증한 기록": false, // one is not, a missing required group ends it
	} {
		holds(t, PlanMatches(text, plan) == want, "PlanMatches(%q) = %v, want %v", text, !want, want)
	}
	holds(t, !PlanMatches("anything at all", MatchPlan{}) && PlanIsEmpty(MatchPlan{}), "an empty plan matches nothing")
	words := []string{"결정", "세션"}
	anyPlan := CompileMatchPlan(ExpandQueryWords(words), words, true, false)
	holds(t, PlanMatches("only a decision here", anyPlan), "any mode needs one hit")
}

// :109.
func TestIsSymbolWord(t *testing.T) {
	for _, w := range []string{"CI", "PR", "FTS", "RRF", "LSP", "go", "id", "ci", "3956", "#3956", "6e97e73d", "hook.ts", "src/hook.ts", "a\\b", "\u212a"} {
		holds(t, IsSymbolWord(w), "%q is symbol-shaped", w)
	}
	for _, w := range []string{"deploy", "release", "Codex", "검색", "배포까지", "트라이그램", "ABCDEFG", "İD", "PR3956"} {
		holds(t, !IsSymbolWord(w), "%q must stay substring-matched", w)
	}
}

// :126, :146 and :297: dots and slashes are boundaries, hits embedded in a word are skipped, a lone v may precede a version.
func TestTermIndexOf(t *testing.T) {
	for _, c := range []struct {
		text string
		term QueryTerm
		want bool
	}{
		{"see hook.ts for details", bounded("hook.ts"), true}, {"edit src/hook.ts now", bounded("src/hook.ts"), true},
		{"edit my-hook.tsx now", bounded("hook.ts"), false}, {"ci", bounded("ci"), true}, {"run ci", bounded("ci"), true},
		{"ci를 돌렸다", bounded("ci"), true}, {"ci_runner", bounded("ci"), false}, {"ci2", bounded("ci"), false},
		{"slsa provenance, v2.49.0", bounded("2.49.0"), true}, {"released (v2.49.0) today", bounded("2.49.0"), true},
		{"v v2.49.0", bounded("2.49.0"), true}, {"av2.49.0", bounded("2.49.0"), false}, {"12.49.0", bounded("2.49.0"), false},
		{"2.49.00", bounded("2.49.0"), false}, {"_v2.49.0", bounded("2.49.0"), false}, {"vv2.49.0", bounded("2.49.0"), false},
		{"vci runner", bounded("ci"), false}, {"😀ci😀", bounded("ci"), true}, {"😀v2.49.0", bounded("2.49.0"), true},
	} {
		holds(t, TermIncludes(c.text, c.term) == c.want, "TermIncludes(%q, %+v) = %v", c.text, c.term, !c.want)
	}
	holds(t, TermIndexOf("nothing here", bounded("ci"), 0) == -1 && TermIndexOf("", bounded("ci"), 0) == -1, "absent")
	holds(t, TermIndexOf("abc", loose(""), 0) == -1 && TermIndexOf("abc", bounded(""), 0) == -1, "an empty term never matches")
	holds(t, TermIndexOf("precision then ci", bounded("ci"), 0) == 15, "the scan goes past the embedded ci")
	holds(t, TermIndexOf("ci ci", bounded("ci"), 1) == 3 && TermIndexOf("ci ci", bounded("ci"), 9) == -1 && TermIndexOf("ci ci", bounded("ci"), -3) == 0, "from")
	holds(t, CountTermOccurrences("precision ci ci_x ci", bounded("ci"), 5) == 2, "boundary count")
	holds(t, CountTermOccurrences("cici cici", loose("ci"), 5) == 4, "substring counts every occurrence")
	holds(t, CountTermOccurrences("ci ci ci ci ci ci", bounded("ci"), 3) == 3 && CountTermOccurrences("ci", bounded("ci"), 0) == 0, "cap holds")
}

// The oracle's offsets are UTF-16 code units; this port's are byte offsets (a Hangul syllable is three bytes and one unit, an astral
// character four bytes and two units).
func TestTermIndexOfCountsBytes(t *testing.T) {
	holds(t, TermIndexOf("가나다 ci", bounded("ci"), 0) == 10, "oracle index 4")
	holds(t, TermIndexOf("😀 ci", bounded("ci"), 0) == 5, "oracle index 3")
}

// :155.
func TestRelaxQueryGroups(t *testing.T) {
	groups := ExpandQueryWords([]string{"LSP"})
	relaxed := RelaxQueryGroups(groups)
	holds(t, HasBoundaryTerm(groups) && !HasBoundaryTerm(relaxed), "relaxing drops the gating")
	wantStrings(t, "only the flag changes", GroupTexts(relaxed[0]), GroupTexts(groups[0])...)
	holds(t, !HasBoundaryTerm(ExpandQueryWords([]string{"배포"})), "korean is never boundary-gated")
	two := ExpandQueryWords([]string{"3956", "LSP"})
	at := RelaxGroupsAt(two, map[int]bool{0: true})
	holds(t, !at[0][0].Boundary && at[1][0].Boundary && two[0][0].Boundary, "only the named group is relaxed, the input is untouched")
}

// :285.
func TestIsVersionWord(t *testing.T) {
	for _, w := range []string{"2.49.0", "2.49", "2.49.0-rc.1", "v2.49.0", "V2.49.0"} {
		holds(t, IsVersionWord(w) && IsSymbolWord(w), "%s is VERSION and stays a symbol", w)
	}
	for _, w := range []string{"hook.ts", "plan.md", "package.json", "src/hook.ts", "LSP", "3956", "deploy", "2.49.", "2.49.0-", "2", "\u00bd.5"} {
		holds(t, !IsVersionWord(w), "%s is not VERSION", w)
	}
	holds(t, IsSymbolWord("hook.ts") && !IsSymbolWord("deploy"), "filename yes, prose no")
}
