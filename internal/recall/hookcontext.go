// CXC v0.2.40 (3c1459ac) recall/src/hook.ts:221-596: cwd recall context.
package recall

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"

type RecallBudget struct {
	Chars   int `json:"chars"`
	TopN    int `json:"topN"`
	Snippet int `json:"snippet"`
}
type HitCountStore interface {
	Read([]string) (map[string]float64, error)
	Bump([]string) error
	Close() error
}
type RecallContextDeps struct {
	SearchChat       func(string, ChatSearchOptions) (ChatSearchResult, error)
	ListCwdSessions  func(string, int) ([]CwdSession, error)
	LoadSummaryIndex func() (map[string]SummaryEntry, error)
	OpenHitCounts    func() (HitCountStore, error)
	Invocation       string
}
type CwdContextOutcome string

const (
	CwdContextHits        CwdContextOutcome = "hits"
	CwdContextEmpty       CwdContextOutcome = "empty"
	CwdContextUnavailable CwdContextOutcome = "unavailable"
)

type CwdContextResult struct {
	Outcome CwdContextOutcome `json:"outcome"`
	Text    string            `json:"text"`
	Detail  string            `json:"detail"`
}

func FullBudget() RecallBudget                           { return RecallBudget{1400, 5, 100} }
func CompactedBudget() RecallBudget                      { return RecallBudget{800, 2, 100} }
func HitCountPenalty(float64) float64                    { return 0 }
func DefaultRecallDeps(host.LookupEnv) RecallContextDeps { return RecallContextDeps{} }
func BuildCwdContext(cwd string, deps RecallContextDeps, budget RecallBudget) string {
	return BuildCwdContextResult(cwd, deps, budget).Text
}
func BuildCwdContextResult(string, RecallContextDeps, RecallBudget) CwdContextResult {
	return CwdContextResult{Outcome: CwdContextEmpty}
}
func RenderCwdBlock(string, [][]string, int, string, string) string { return "" }
func hookContextUnits(string, *cwdExcerptClip) []uint16             { return nil }
func hookContextQuote([]uint16) string                              { return "" }
func hookContextClip(u []uint16, _ int) []uint16                    { return u }
func hookContextSliceEnd(n, end int) int {
	if end < 0 {
		end += n
	}
	return max(0, min(n, end))
}
func hookContextCandidatePool(n int, _ RecallContextDeps) int { return n }
func hookContextDemote[T any](c []T, n int, _ func(T) string, _ RecallContextDeps) []T {
	return c[:hookContextSliceEnd(len(c), n)]
}
