// CXC v0.2.40 recall/src/chat-search.ts:46-150,269-390: library scan path.
package recall

import "time"

const (
	DefaultDays                = 7
	DefaultLimit               = 50
	MaxLimit                   = 200
	RolloutAll   RolloutSource = "all"
)

// ChatOrder is shared with the later index engine; scans always sort by recency.
type ChatOrder string

const (
	ChatRelevance ChatOrder = "relevance"
	ChatRecent    ChatOrder = "recent"
)

// ChatSearchOptions preserves absent numbers/strings separately from explicit zero/empty.
// NowMs, Order, IndexPath and NoRefresh are index options, unused by the scan.
type ChatSearchOptions struct {
	Days             *float64       `json:"days,omitempty"`
	Limit            *float64       `json:"limit,omitempty"`
	Context          *float64       `json:"context,omitempty"`
	Any              bool           `json:"any,omitempty"`
	Synonyms         bool           `json:"synonyms,omitempty"`
	Role             *string        `json:"role,omitempty"`
	Cwd              *string        `json:"cwd,omitempty"`
	Source           *RolloutSource `json:"source,omitempty"`
	IncludeSynthetic bool           `json:"includeSynthetic,omitempty"`
	IncludeTools     *bool          `json:"includeTools,omitempty"`
	Home             *string        `json:"home,omitempty"`
	Scan             bool           `json:"scan,omitempty"`
	NoRefresh        bool           `json:"noRefresh,omitempty"`
	Order            ChatOrder      `json:"order,omitempty"`
	NowMs            *float64       `json:"nowMs,omitempty"`
	IndexPath        *string        `json:"indexPath,omitempty"`
	ReadOriginUrl    ReadOriginUrl  `json:"-"`
}

type ChatContextEntry struct {
	TS      string `json:"ts"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	IsMatch bool   `json:"isMatch"`
}

type ChatHit struct {
	TS         string             `json:"ts"`
	Role       string             `json:"role"`
	Text       string             `json:"text"`
	MatchField string             `json:"matchField"`
	ThreadID   *string            `json:"threadId"`
	Title      *string            `json:"title"`
	Cwd        *string            `json:"cwd"`
	GitBranch  *string            `json:"gitBranch"`
	Source     RolloutSource      `json:"source"`
	File       string             `json:"file"`
	Score      *float64           `json:"score,omitempty"`
	Context    []ChatContextEntry `json:"context"`
}

type ChatIndexInfo struct {
	LastIngestAt *string `json:"lastIngestAt"`
	Files        int     `json:"files"`
	SourceFiles  int     `json:"sourceFiles"`
	StaleFiles   int     `json:"staleFiles"`
	ReadOnly     bool    `json:"readOnly"`
}

type ChatSearchResult struct {
	Hits         []ChatHit      `json:"hits"`
	Warnings     []string       `json:"warnings"`
	ScannedFiles int            `json:"scannedFiles"`
	MatchedFiles int            `json:"matchedFiles"`
	TotalFiles   int            `json:"totalFiles"`
	ElapsedMs    int64          `json:"elapsedMs"`
	Mode         string         `json:"mode"`
	Index        *ChatIndexInfo `json:"index,omitempty"`
}

// chatScanShared is resolved by the future searchChat dispatcher, not by the scan.
type chatScanShared struct {
	Home                  string
	Days, Limit, ContextN float64
	Plan                  MatchPlan
	Source                RolloutSource
	RepoKey               string
}

func ChatMatchPlan(query string, anyMode, synonyms bool) MatchPlan {
	return MatchPlan{Required: []QueryGroup{}, Optional: []QueryGroup{}}
}

func searchViaScan(_ string, opts ChatSearchOptions, shared chatScanShared, clock ...time.Time) (ChatSearchResult, error) {
	return ChatSearchResult{Hits: []ChatHit{}, Warnings: []string{}, Mode: "scan"}, nil
}

func contextWindow(entries []ChatEntry, index int, n float64) ([]ChatContextEntry, error) {
	return []ChatContextEntry{}, nil
}

func chatScanCutoff(now time.Time, days float64) (string, error) { return "", nil }
