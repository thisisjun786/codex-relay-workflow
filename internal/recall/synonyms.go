package recall

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// synonymGroups is the oracle's SYNONYM_GROUPS (synonyms.ts:31-66): one concept per group. A word's group is found by scanning, the
// oracle's map while no member repeats and every member is lowercase (a test pins both).
var synonymGroups = [][]string{
	// cli-jaw seeds
	{"preference", "preferences", "선호", "취향", "환경설정"},
	{"decision", "decisions", "결정", "선택", "방침"},
	{"project", "projects", "프로젝트", "작업"},
	{"runbook", "runbooks", "절차", "런북", "매뉴얼"},
	{"workflow", "워크플로우", "흐름"},
	{"pabcd", "plan", "audit", "build", "check", "done"},
	{"fts", "fts5", "full-text-search"},
	{"bm25", "ranking", "relevance"},
	{"cli-jaw", "cli_jaw", "clijaw", "jaw"},
	// codexclaw-domain additions
	{"memory", "memories", "메모리", "기억"},
	{"search", "검색"},
	{"session", "sessions", "세션"},
	{"error", "errors", "오류", "에러", "bug", "버그"},
	{"test", "tests", "테스트"},
	{"skill", "skills", "스킬"},
	{"plugin", "plugins", "플러그인"},
	{"index", "인덱스"},
	{"hook", "hooks", "훅"},
	{"config", "configuration", "설정"},
	{"deploy", "deployment", "배포"},
	{"release", "releases", "릴리스", "릴리즈"},
	{"commit", "commits", "커밋"},
	{"review", "reviews", "리뷰", "검토"},
	{"branch", "branches", "브랜치"},
	// 260910 wp5 seeds
	{"dogfooding", "도그푸딩"},
	{"codex", "코덱스"},
	{"restart", "재시작"},
	{"verify", "verification", "verified", "검증", "provenance"},
	{"source", "소스"},
}

// SynonymGroups is SYNONYM_GROUPS, as a copy.
func SynonymGroups() [][]string {
	out := make([][]string, len(synonymGroups))
	for i, g := range synonymGroups {
		out[i] = slices.Clone(g)
	}
	return out
}

const (
	groupCap         = 8 // members per expanded group: the original word plus its stem and synonyms
	minStemSyllables = 2 // a trimmed stem shorter than this is rejected: 검사 -> 검 explodes recall
)

// koreanEndings are particles and verbalizer endings, longest first so 에서는 never loses to 는 (synonyms.ts:81-120, already in
// that order). 한 stays out: it would trim 검증한 to 검증 but also every noun ending in 한.
var koreanEndings = [...]string{
	"에서는", "으로는",
	"에서", "으로", "에는", "이나", "까지", "부터", "처럼", "보다", "마다", "라고", "하고", "해서", "하는", "한테", "인지", "들을", "들이", "에게",
	"을", "를", "이", "가", "은", "는", "의", "에", "도", "과", "와", "만", "로",
}

func isHangulOnly(word string) bool {
	for _, r := range word {
		if r < 0xac00 || r > 0xd7a3 {
			return false
		}
	}
	return word != ""
}

// KoreanStem is the Korean stem of a word, or false when nothing safe can be trimmed: Hangul-only input, a stem of at least two
// syllables, and the original is never removed (callers keep it). An ending that would leave a shorter stem is skipped for the next
// one (집에서는 gives 집에서), and the conjugated 하-verbalizer tail (결정했지, 배포하고) is judged at its leftmost position only
// (해결했지 gives none).
func KoreanStem(word string) (string, bool) {
	if !isHangulOnly(word) {
		return "", false
	}
	for _, ending := range koreanEndings {
		if stem, ok := strings.CutSuffix(word, ending); ok && utf8.RuneCountInString(stem) >= minStemSyllables {
			return stem, true
		}
	}
	rs := []rune(word)
	for i := max(len(rs)-4, 0); i < len(rs); i++ { // HA_VERB_TAIL: 했|하|해 followed by at most three syllables
		if r := rs[i]; r == '했' || r == '하' || r == '해' {
			if i >= minStemSyllables {
				return string(rs[:i]), true
			}
			return "", false
		}
	}
	return "", false
}

func synonymsOf(table [][]string, key string) []string {
	for _, g := range table {
		if slices.Contains(g, key) {
			return g
		}
	}
	return nil
}

// ExpandQueryWords expands query words into OR-groups of matchable terms. Words arrive with their original case because the symbol
// judgment needs it (CI against a bare ci fragment); every emitted text is lowercase. The original word leads its group, then its
// Korean stem, then the synonyms of the raw word and of the stem (so 배포를 reaches deploy), deduplicated and capped at groupCap.
// Unknown words become singleton groups.
func ExpandQueryWords(words []string) []QueryGroup { return expand(words, synonymGroups) }

func expand(words []string, table [][]string) []QueryGroup {
	groups := make([]QueryGroup, len(words))
	for i, word := range words {
		lower := Lower(word)
		texts, keys := []string{lower}, []string{lower}
		if stem, ok := KoreanStem(lower); ok {
			texts, keys = append(texts, stem), append(keys, stem)
		}
		for _, key := range keys {
			for _, member := range synonymsOf(table, key) {
				if len(texts) < groupCap && !slices.Contains(texts, member) {
					texts = append(texts, member)
				}
			}
		}
		boundary := IsSymbolWord(word)
		groups[i] = make(QueryGroup, len(texts))
		for j, text := range texts {
			groups[i][j] = QueryTerm{Text: text, Boundary: boundary}
		}
	}
	return groups
}
