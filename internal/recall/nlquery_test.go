package recall

import "testing"

// The plan assertions of CXC v0.2.40 recall/test/nl-query.test.ts (the wp5 natural-language golden set); its chat, index, memory and
// CLI assertions need the engines of later ports. The helpers mirror the plan compilation in chat-search.ts:145 (boundary gating off
// on every chat term) and memory-search.ts:437-470 (expansion on by default, off keeps each group's first term).

const (
	sentenceD3 = "지난번 로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인한 방법"
	sentenceP2 = "코덱스를 재시작하면 플러그인이 사라지는 문제"
	sentenceR2 = "2.49.0 배포하고 npm 패키지가 진짜 그 소스인지 검증한 기록"
)

func chatMatchPlan(query string, anyMode, synonyms bool) MatchPlan {
	rawAll := SplitQueryWordsRaw(query)
	raw := DropStopwords(rawAll)
	groups := []QueryGroup{}
	for _, w := range raw {
		groups = append(groups, QueryGroup{{Text: Lower(w)}})
	}
	if synonyms {
		groups = RelaxQueryGroups(ExpandQueryWords(raw))
	}
	return CompileMatchPlan(groups, raw, anyMode, len(rawAll) > MaxWords)
}

func memoryMatchPlan(query string, anyMode, synonyms bool) MatchPlan {
	rawAll := SplitQueryWordsRaw(query)
	words := DropStopwords(rawAll)
	groups := ExpandQueryWords(words)
	if !synonyms {
		for i, g := range groups {
			groups[i] = g[:1]
		}
	}
	return CompileMatchPlan(groups, words, anyMode, len(rawAll) > MaxWords)
}

// G-D3 (:76): ten words, 방법 dropped, no required term, a quota of five of nine. G-P2 (:99): five words stay a strict AND. G-R2
// (:120): the version and the package manager are required, the padding is a quota, in chat and in memory alike.
func TestNLGoldenPlans(t *testing.T) {
	d3 := chatMatchPlan(sentenceD3, false, false)
	holds(t, len(d3.Optional) == 9 && len(d3.Required) == 0 && d3.MinOptional == 5, "D3: %+v", d3)
	p2 := chatMatchPlan(sentenceP2, false, false)
	holds(t, len(p2.Optional) == 0 && len(p2.Required) == 4, "P2: %+v", p2)
	for _, plan := range []MatchPlan{chatMatchPlan(sentenceR2, false, false), memoryMatchPlan(sentenceR2, false, true)} {
		wantStrings(t, "R2 required", leads(plan.Required), "2.49.0", "npm")
		holds(t, len(plan.Optional) == 6 && plan.MinOptional == 3, "R2: %+v", plan)
	}
}

// N-empty (:164): an absent term matches nothing, and a query of nothing but stopwords keeps its words instead of relaxing into
// "match everything".
func TestNLEmptyQueries(t *testing.T) {
	absent := chatMatchPlan("zxqv84721무지개잠수함", false, false)
	holds(t, !PlanIsEmpty(absent) && !PlanMatches("the quick brown fox 무지개", absent), "an absent term matches nothing")
	stops := memoryMatchPlan("그 이 저 것 문제 방법", false, true)
	holds(t, len(stops.Required) == 6 && !PlanMatches("any text at all", stops), "stopwords only: %+v", stops)
	holds(t, PlanIsEmpty(memoryMatchPlan("   ", false, true)), "an empty query is an empty plan")
}
