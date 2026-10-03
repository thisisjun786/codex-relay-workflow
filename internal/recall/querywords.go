// Package recall is the Go form of CXC v0.2.40 recall/src/query-words.ts and synonyms.ts (commit 3c1459ac): how a typed query becomes
// words, which words match on token boundaries or are required, what a match plan asks of a text, and the ko/en synonym groups with
// Korean ending trimming. A library; no command or hook calls it yet.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md); the oracle's regular expressions are written
// out by hand, since no package variable may do work at start-up. JavaScript semantics are reproduced where they reach a decision:
// the \s white space and toLowerCase (Lower). Only well-formed UTF-8 text is in the parity domain (a lone UTF-16 surrogate has no Go
// string form) and the Unicode tables are Go's, which can differ from the oracle's runtime on a recently assigned character.
//
// Slices the oracle returns as arrays are never nil, so a serialized plan or group list keeps the oracle's [] shape.
//
// Offsets are byte offsets where the oracle's are UTF-16 code units: they agree on ASCII text and differ behind any other character.
// The boundary test reads single bytes as the oracle reads single units: a byte of 0x80 or more is no token character, as no
// non-ASCII unit is.
package recall

import (
	"slices"
	"strings"
)

// MaxWords is a relaxation threshold, not a truncation cap (see CompileMatchPlan); MaxQueryTerms is the hard tokenizer cap.
const (
	MaxWords      = 8
	MaxQueryTerms = 16
)

// QueryTerm is one matchable term: lowercase text plus whether it must land on a token boundary.
type QueryTerm struct {
	Text     string `json:"text"`
	Boundary bool   `json:"boundary"`
}

// QueryGroup is an OR-group of interchangeable terms; matching stays AND across groups. Groups are shared, treat them as immutable.
type QueryGroup []QueryTerm

// MatchPlan is what a text has to carry: every Required group and MinOptional of the Optional ones, or, with AnyMode, one group of either.
type MatchPlan struct {
	Required    []QueryGroup `json:"required"`
	Optional    []QueryGroup `json:"optional"`
	MinOptional int          `json:"minOptional"`
	AnyMode     bool         `json:"anyMode"`
}

// SplitQueryWordsRaw is query.split(/\s+/) without empty words, cut at MaxQueryTerms, with the original case.
func SplitQueryWordsRaw(query string) []string {
	words := strings.FieldsFunc(query, isJSSpace)
	return words[:min(len(words), MaxQueryTerms)]
}

// SplitQueryWords is SplitQueryWordsRaw lowercased.
func SplitQueryWords(query string) []string {
	words := SplitQueryWordsRaw(query)
	for i, w := range words {
		words[i] = Lower(w)
	}
	return words
}

func isLower(b byte) bool    { return 'a' <= b && b <= 'z' }
func isUpper(b byte) bool    { return 'A' <= b && b <= 'Z' }
func isDigit(b byte) bool    { return '0' <= b && b <= '9' }
func isAlnum(b byte) bool    { return isLower(b) || isUpper(b) || isDigit(b) }
func isLowerHex(b byte) bool { return isDigit(b) || 'a' <= b && b <= 'f' }

// isTokenChar is TOKEN_CHAR /[A-Za-z0-9_]/, narrower than \b: dots, slashes and hyphens are boundaries.
func isTokenChar(b byte) bool { return isAlnum(b) || b == '_' }

// anyByte and allBytes test ASCII characters; a character outside ASCII satisfies no class.
func anyByte(s string, ok func(byte) bool) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x80 && ok(byte(r)) }) >= 0
}

func allBytes(s string, ok func(byte) bool) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r >= 0x80 || !ok(byte(r)) }) < 0
}

func sized(s string, lo, hi int, ok func(byte) bool) bool {
	return lo <= len(s) && len(s) <= hi && allBytes(s, ok)
}

func digitsFrom(s string, i int) int {
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}

// isVersionCore is VERSION_CORE /^\d+\.\d+(?:\.\d+)*(?:-[a-z0-9.]+)?$/; the grammar needs no backtracking.
func isVersionCore(s string) bool {
	i := digitsFrom(s, 0)
	if i == 0 || i >= len(s) || s[i] != '.' {
		return false
	}
	if j := digitsFrom(s, i+1); j > i+1 {
		i = j
	} else {
		return false
	}
	for i < len(s) && s[i] == '.' && digitsFrom(s, i+1) > i+1 {
		i = digitsFrom(s, i+1)
	}
	return i == len(s) || s[i] == '-' && i+1 < len(s) && allBytes(s[i+1:], func(b byte) bool { return isLower(b) || isDigit(b) || b == '.' })
}

// IsVersionWord is VERSION: a dotted version with an optional leading v (2.49, 2.49.0, v2.49.0-rc.1).
func IsVersionWord(rawWord string) bool {
	return isVersionCore(strings.TrimPrefix(Lower(rawWord), "v"))
}

// IsSymbolWord reports whether a word is symbol-shaped, i.e. matches on token boundaries; it takes the word as typed (CI is an acronym).
func IsSymbolWord(rawWord string) bool {
	if sized(rawWord, 2, 6, isUpper) { // UPPER_ACRONYM
		return true
	}
	lower := Lower(rawWord)
	ext := lower[strings.LastIndexByte(lower, '.')+1:]
	return isVersionCore(strings.TrimPrefix(lower, "v")) ||
		sized(lower, 1, 3, isLower) || // SHORT_ASCII
		sized(strings.TrimPrefix(lower, "#"), 2, 10, isDigit) || // NUMERIC_ID
		sized(lower, 7, 40, isLowerHex) || // SHA
		strings.Contains(lower, ".") && sized(ext, 1, 5, func(b byte) bool { return isLower(b) || isDigit(b) }) || // FILENAME
		strings.ContainsAny(lower, "/\\") // PATH_LIKE
}

// IsRequiredTerm reports whether a word must be present rather than count toward the optional quota: a symbol, or a mixed-case ASCII
// proper noun (Codex). It takes the word as typed.
func IsRequiredTerm(rawWord string) bool {
	return IsSymbolWord(rawWord) || rawWord != "" && !isDigit(rawWord[0]) && allBytes(rawWord, isAlnum) && anyByte(rawWord, isUpper) && anyByte(rawWord, isLower)
}

func isBoundaryAt(lowerText string, at, length int) bool {
	var before, after, beforeV byte // 0 stands for the edge of the text; neither is a token character
	if at > 0 {
		before = lowerText[at-1]
	}
	if at+length < len(lowerText) {
		after = lowerText[at+length]
	}
	if !isTokenChar(before) && !isTokenChar(after) {
		return true
	}
	// v2.49.0 / (v2.49.0): a lone v right before a VERSION core is a boundary iff that v itself sits on a token edge.
	if before == 'v' && !isTokenChar(after) && isVersionCore(lowerText[at:at+length]) {
		if at >= 2 {
			beforeV = lowerText[at-2]
		}
		return !isTokenChar(beforeV)
	}
	return false
}

func indexFrom(s, sub string, from int) int {
	from = max(from, 0)
	if from > len(s) {
		return -1
	}
	if i := strings.Index(s[from:], sub); i >= 0 {
		return from + i
	}
	return -1
}

// TermIndexOf is the byte offset of the term in already-lowercased text from offset from, honoring its boundary flag, or -1; an empty
// term never matches. Matching, counting and excerpt anchoring are all built on it.
func TermIndexOf(lowerText string, term QueryTerm, from int) int {
	if term.Text == "" {
		return -1
	}
	at := indexFrom(lowerText, term.Text, from)
	for term.Boundary && at != -1 && !isBoundaryAt(lowerText, at, len(term.Text)) {
		at = indexFrom(lowerText, term.Text, at+1)
	}
	return at
}

// TermIncludes reports whether the term occurs in the lowercased text under its own boundary rule.
func TermIncludes(lowerText string, term QueryTerm) bool {
	return TermIndexOf(lowerText, term, 0) != -1
}

// CountTermOccurrences counts occurrences for density scoring, stopping at limit.
func CountTermOccurrences(lowerText string, term QueryTerm, limit int) int {
	n := 0
	for at := TermIndexOf(lowerText, term, 0); at != -1 && n < limit; at = TermIndexOf(lowerText, term, at+len(term.Text)) {
		n++
	}
	return n
}

// HasBoundaryTerm reports whether any term of any group is boundary-gated (it drives the relaxed retry).
func HasBoundaryTerm(groups []QueryGroup) bool {
	return slices.ContainsFunc(groups, func(g QueryGroup) bool { return slices.ContainsFunc(g, func(t QueryTerm) bool { return t.Boundary }) })
}

// RelaxQueryGroups is the groups with boundary gating dropped, for the zero-result retry.
func RelaxQueryGroups(groups []QueryGroup) []QueryGroup {
	all := make(map[int]bool, len(groups))
	for i := range groups {
		all[i] = true
	}
	return RelaxGroupsAt(groups, all)
}

// RelaxGroupsAt drops boundary gating only for the groups at indexes; the other groups are shared with the input.
func RelaxGroupsAt(groups []QueryGroup, indexes map[int]bool) []QueryGroup {
	out := slices.Clone(groups)
	for i, g := range groups {
		if indexes[i] {
			out[i] = make(QueryGroup, len(g))
			for j, t := range g {
				out[i][j] = QueryTerm{Text: t.Text}
			}
		}
	}
	return out
}

// GroupTexts is the plain member texts of a group.
func GroupTexts(group QueryGroup) []string {
	texts := make([]string, len(group))
	for i, t := range group {
		texts[i] = t.Text
	}
	return texts
}

// queryStopwords are six words measured as padding the user never meant as a search term.
var queryStopwords = [...]string{"그", "이", "저", "것", "문제", "방법"}

// QueryStopwords is QUERY_STOPWORDS, as a copy.
func QueryStopwords() []string { return slices.Clone(queryStopwords[:]) }

// DropStopwords drops stopwords, except a required-shaped word (CI is the whole query) and when nothing else would be left: a query of
// stopwords only keeps its words instead of degenerating into "match everything".
func DropStopwords(rawWords []string) []string {
	kept := make([]string, 0, len(rawWords))
	for _, w := range rawWords {
		if !slices.Contains(queryStopwords[:], Lower(w)) || IsRequiredTerm(w) {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return rawWords
	}
	return kept
}

// CompileMatchPlan builds the plan. relax is the caller's decision, taken from the ORIGINAL token count against MaxWords before
// stopword removal; rawWords is index-aligned with groups, since the shape judgment needs the word as typed.
func CompileMatchPlan(groups []QueryGroup, rawWords []string, anyMode, relax bool) MatchPlan {
	if anyMode || !relax {
		return MatchPlan{Required: groups, Optional: []QueryGroup{}, AnyMode: anyMode}
	}
	plan := MatchPlan{Required: []QueryGroup{}, Optional: []QueryGroup{}}
	for i, g := range groups {
		raw := ""
		if i < len(rawWords) {
			raw = rawWords[i]
		} else if len(g) > 0 {
			raw = g[0].Text
		}
		if IsRequiredTerm(raw) {
			plan.Required = append(plan.Required, g)
		} else {
			plan.Optional = append(plan.Optional, g)
		}
	}
	plan.MinOptional = (len(plan.Optional) + 1) / 2
	return plan
}

// AllGroups is every group of the plan, required first.
func AllGroups(plan MatchPlan) []QueryGroup {
	if len(plan.Optional) == 0 {
		return plan.Required
	}
	return slices.Concat(plan.Required, plan.Optional)
}

// PlanIsEmpty reports a plan with no groups at all, an empty query, which must match nothing.
func PlanIsEmpty(plan MatchPlan) bool { return len(plan.Required) == 0 && len(plan.Optional) == 0 }

// PlanMatches is the single match predicate every engine ends in.
func PlanMatches(lowerText string, plan MatchPlan) bool {
	hit := func(g QueryGroup) bool {
		return slices.ContainsFunc(g, func(t QueryTerm) bool { return TermIncludes(lowerText, t) })
	}
	if plan.AnyMode {
		return slices.ContainsFunc(plan.Required, hit) || slices.ContainsFunc(plan.Optional, hit)
	}
	if PlanIsEmpty(plan) {
		return false
	}
	for _, g := range plan.Required {
		if !hit(g) {
			return false
		}
	}
	if len(plan.Optional) == 0 {
		return true
	}
	seen := 0
	for _, g := range plan.Optional {
		if hit(g) {
			if seen++; seen >= plan.MinOptional {
				return true
			}
		}
	}
	return seen >= plan.MinOptional
}
