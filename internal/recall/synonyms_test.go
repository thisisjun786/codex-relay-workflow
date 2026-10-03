package recall

import (
	"slices"
	"testing"
)

// Ports of the B-class tests of CXC v0.2.40 recall/test/synonyms.test.ts whose subject is synonyms.ts (the eight that call
// searchMemory or scoreChunk belong to the memory-search port).

func texts(words ...string) (out []string) {
	for _, g := range ExpandQueryWords(words) {
		out = append(out, GroupTexts(g)...)
	}
	return out
}

func stemIs(t *testing.T, in, want string) {
	t.Helper()
	got, ok := KoreanStem(in)
	holds(t, (ok && got == want) || (!ok && want == "" && got == ""), "KoreanStem(%q) = %q, %v, want %q", in, got, ok, want)
}

// :23: groups lead with the original word, unknown words stay singletons, the cap holds.
func TestExpandQueryWords(t *testing.T) {
	groups := ExpandQueryWords([]string{"결정", "quagga"})
	holds(t, len(groups) == 2 && groups[0][0].Text == "결정" && slices.Contains(GroupTexts(groups[0]), "decision"), "%+v", groups)
	wantStrings(t, "unknown word", GroupTexts(groups[1]), "quagga")
}

// :23, the cap: no word of the shipped table reaches eight members (the oracle's longest group is 7, recorded), so the truncating
// branch is exercised on a synthetic table.
func TestExpandCapsAGroupAtEightMembers(t *testing.T) {
	table := [][]string{{"zz", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9"}}
	wantStrings(t, "capped", GroupTexts(expand([]string{"zz"}, table)[0]), "zz", "m1", "m2", "m3", "m4", "m5", "m6", "m7")
	wantStrings(t, "an empty word", GroupTexts(ExpandQueryWords([]string{""})[0]), "")
}

// :34 and :61: measured endings trim, every over-trimming guard refuses.
func TestKoreanStem(t *testing.T) {
	for in, want := range map[string]string{
		"배포까지": "배포", "문서를": "문서", "인덱스도": "인덱스", "스킬들을": "스킬", "세션에서는": "세션", "결정했지": "결정", "배포하고": "배포", "검색해야": "검색",
		"소스인지": "소스", "무엇인지": "무엇",
	} {
		stemIs(t, in, want)
	}
	for _, in := range []string{"검사", "고의", "하고", "이해", "오해", "릴리스", "도구", "deploy", "hook.ts", "배포2", "", "인지", "검증한"} {
		stemIs(t, in, "")
	}
}

// :71: the evaluation sentences' nouns reach their english family; :95: the stem is additive and looked up again in the table.
func TestExpandQueryWordsReachesTheEnglishFamily(t *testing.T) {
	for in, want := range map[string]string{"도그푸딩": "dogfooding", "코덱스를": "codex", "재시작하면": "restart", "소스인지": "source", "검증": "provenance", "배포를": "deployment"} {
		holds(t, slices.Contains(texts(in), want), "%s must reach %s: %q", in, want, texts(in))
	}
	wantStrings(t, "검증한", texts("검증한"), "검증한") // no 한 trimming means no seed lookup either
	holds(t, texts("배포를")[0] == "배포를" && slices.Contains(texts("배포를"), "배포"), "the typed word leads, the stem is added")
	holds(t, !ExpandQueryWords([]string{"배포를"})[0][0].Boundary && ExpandQueryWords([]string{"CI"})[0][0].Boundary, "boundary follows the typed word")
}

// What the oracle does and the port keeps (known-defects.md): a too-short stem falls through to a shorter ending, and the 하/해 tail
// is judged at its leftmost position only.
func TestKoreanStemOracleQuirks(t *testing.T) {
	stemIs(t, "집에서는", "집에서")
	stemIs(t, "결정하하하하", "결정")
	for _, in := range []string{"해결했지", "해결하다", "오하해하", "하하하하하"} {
		stemIs(t, in, "")
	}
}

// ExpandQueryWords scans the table instead of building a map at start-up, which is the oracle's lookup only while no member repeats
// and every member is already lowercase.
func TestSynonymGroupsSuitTheLinearLookup(t *testing.T) {
	seen := map[string]bool{}
	groups := SynonymGroups()
	for _, g := range groups {
		for _, m := range g {
			holds(t, Lower(m) == m && !seen[m], "member %q is repeated or not lowercase", m)
			seen[m] = true
		}
	}
	groups[0][0] = "changed"
	holds(t, len(groups) == 29 && SynonymGroups()[0][0] == "preference", "29 groups, a copy is returned")
}
