// Package recall (red stage: signatures only, replaced by the port).
package recall

const (
	MaxWords      = 8
	MaxQueryTerms = 16
)

type QueryTerm struct {
	Text     string `json:"text"`
	Boundary bool   `json:"boundary"`
}
type QueryGroup []QueryTerm
type MatchPlan struct {
	Required    []QueryGroup `json:"required"`
	Optional    []QueryGroup `json:"optional"`
	MinOptional int          `json:"minOptional"`
	AnyMode     bool         `json:"anyMode"`
}

var synonymGroups [][]string

func SplitQueryWordsRaw(query string) []string                             { return nil }
func SplitQueryWords(query string) []string                                { return nil }
func IsVersionWord(rawWord string) bool                                    { return false }
func IsSymbolWord(rawWord string) bool                                     { return false }
func TermIndexOf(lowerText string, term QueryTerm, from int) int           { return -2 }
func TermIncludes(lowerText string, term QueryTerm) bool                   { return false }
func CountTermOccurrences(lowerText string, term QueryTerm, limit int) int { return -1 }
func HasBoundaryTerm(groups []QueryGroup) bool                             { return false }
func RelaxQueryGroups(groups []QueryGroup) []QueryGroup                    { return nil }
func RelaxGroupsAt(groups []QueryGroup, indexes map[int]bool) []QueryGroup { return nil }
func GroupTexts(group QueryGroup) []string                                 { return nil }
func QueryStopwords() []string                                             { return nil }
func IsRequiredTerm(rawWord string) bool                                   { return false }
func DropStopwords(rawWords []string) []string                             { return nil }
func CompileMatchPlan(groups []QueryGroup, rawWords []string, anyMode, relax bool) MatchPlan {
	return MatchPlan{}
}
func AllGroups(plan MatchPlan) []QueryGroup                { return nil }
func PlanIsEmpty(plan MatchPlan) bool                      { return false }
func PlanMatches(lowerText string, plan MatchPlan) bool    { return false }
func Lower(s string) string                                { return s }
func SynonymGroups() [][]string                            { return nil }
func KoreanStem(word string) (string, bool)                { return "", false }
func ExpandQueryWords(words []string) []QueryGroup         { return nil }
func expand(words []string, table [][]string) []QueryGroup { return nil }
