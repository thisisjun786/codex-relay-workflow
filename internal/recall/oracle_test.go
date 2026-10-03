package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// testdata/oracle-query-words.json holds what the CXC v0.2.40 oracle's query-words.ts and synonyms.ts answered over the grids of
// testdata/record-oracle.mjs, recorded once under Node 24 (no Node runs here). Every case is {fn, in, out}: the oracle function of
// that name applied to the arguments. The oracle's offsets are UTF-16 code units and this package's are bytes, so a "term" case is
// compared through utf16Index and a "termFrom" case, whose texts are ASCII, directly. A lone surrogate cannot be in a Go string, so
// no grid holds one.

type oracleCase struct {
	Fn  string
	In  []json.RawMessage
	Out json.RawMessage
}

func arg[T any](t *testing.T, c oracleCase, i int) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(c.In[i], &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// canon reads v through JSON, so that a Go nil slice (null) differs from the oracle's [].
func canon(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// utf16Index is the oracle's index of the byte offset at in s.
func utf16Index(s string, at int) int {
	if at < 0 {
		return -1
	}
	n := 0
	for _, r := range s[:at] {
		if n++; r >= 0x10000 {
			n++
		}
	}
	return n
}

func TestOracle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-query-words.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []oracleCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, c := range cases {
		seen[c.Fn]++
		str, words := func() string { return arg[string](t, c, 0) }, func() []string { return arg[[]string](t, c, 0) }
		var got any
		switch c.Fn {
		case "consts":
			got = []int{MaxWords, MaxQueryTerms}
		case "stopwords":
			got = QueryStopwords()
		case "synonymGroups":
			got = SynonymGroups()
		case "splitRaw":
			got = SplitQueryWordsRaw(str())
		case "split":
			got = SplitQueryWords(str())
		case "symbol":
			got = IsSymbolWord(str())
		case "version":
			got = IsVersionWord(str())
		case "required":
			got = IsRequiredTerm(str())
		case "stem":
			if stem, ok := KoreanStem(str()); ok {
				got = stem
			}
		case "expand":
			got = ExpandQueryWords(words())
		case "dropStopwords":
			got = DropStopwords(words())
		case "term":
			text, term := str(), arg[QueryTerm](t, c, 1)
			got = []any{utf16Index(text, TermIndexOf(text, term, 0)), TermIncludes(text, term), CountTermOccurrences(text, term, 5), CountTermOccurrences(text, term, 1), CountTermOccurrences(text, term, 0)}
		case "termFrom":
			got = TermIndexOf(str(), arg[QueryTerm](t, c, 1), arg[int](t, c, 2))
		case "plan":
			plan := chatMatchPlan
			if str() == "memory" {
				plan = memoryMatchPlan
			}
			p, hits := plan(arg[string](t, c, 1), arg[bool](t, c, 2), arg[bool](t, c, 3)), []bool{}
			for _, text := range arg[[]string](t, c, 4) {
				hits = append(hits, PlanMatches(text, p))
			}
			got = map[string]any{"plan": p, "empty": PlanIsEmpty(p), "all": AllGroups(p), "hits": hits}
		case "planMatches":
			hits := []bool{}
			for _, text := range arg[[]string](t, c, 1) {
				hits = append(hits, PlanMatches(text, arg[MatchPlan](t, c, 0)))
			}
			got = hits
		case "compile":
			got = CompileMatchPlan(arg[[]QueryGroup](t, c, 0), arg[[]string](t, c, 1), arg[bool](t, c, 2), true)
		case "maxGroup": // the longest group over every member plus each ending of the case
			best := 0
			for _, g := range synonymGroups {
				for _, m := range g {
					for _, ending := range arg[[]string](t, c, 0) {
						best = max(best, len(ExpandQueryWords([]string{m + ending})[0]))
					}
				}
			}
			got = best
		case "relax":
			groups := ExpandQueryWords(words())
			relaxed, texts, ats := RelaxQueryGroups(groups), [][]string{}, [][]QueryGroup{}
			for _, g := range groups {
				texts = append(texts, GroupTexts(g))
			}
			for _, idx := range arg[[][]int](t, c, 1) {
				set := map[int]bool{}
				for _, i := range idx {
					set[i] = true
				}
				ats = append(ats, RelaxGroupsAt(groups, set))
			}
			got = []any{HasBoundaryTerm(groups), HasBoundaryTerm(relaxed), relaxed, texts, ats}
		default:
			t.Fatalf("unknown oracle function %q", c.Fn)
		}
		var want any
		if err := json.Unmarshal(c.Out, &want); err != nil {
			t.Fatal(err)
		}
		if g := canon(t, got); !reflect.DeepEqual(g, want) {
			t.Errorf("%s%s: got %v, oracle %v", c.Fn, c.In, g, want)
		}
	}
	if len(seen) != 18 || len(cases) < 3000 {
		t.Errorf("the oracle file holds %d cases of %d functions, want 18 functions and 3000 cases or more", len(cases), len(seen))
	}
}
