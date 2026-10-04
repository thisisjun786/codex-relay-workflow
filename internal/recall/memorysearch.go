package recall

// ChatSearchFn uses the already-landed chat result and option contracts.
type ChatSearchFn func(string, ChatSearchOptions) (ChatSearchResult, error)

// MemorySearchOptions ports recall/src/memory-search.ts:49-77. The three chat
// fields are successor integration seams; this port searches markdown only.
type MemorySearchOptions struct {
	Limit             *float64      `json:"limit,omitempty"`
	Days              *float64      `json:"days,omitempty"`
	Any               bool          `json:"any,omitempty"`
	Home              *string       `json:"home,omitempty"`
	Synonyms          *bool         `json:"synonyms,omitempty"`
	NowMs             *float64      `json:"nowMs,omitempty"`
	Cwd               *string       `json:"cwd,omitempty"`
	CwdOnly           bool          `json:"cwdOnly,omitempty"`
	ReadOriginUrl     ReadOriginUrl `json:"-"`
	SearchChat        ChatSearchFn  `json:"-"`
	ChatFallbackBelow *float64      `json:"chatFallbackBelow,omitempty"`
	ChatIncludeTools  bool          `json:"chatIncludeTools,omitempty"`
}

// CwdScope is the per-search path/origin scope from memory-search.ts:292-304.
type CwdScope struct {
	prefix        string
	lowerPrefixes [2]string
	only          bool
	threadCwd     map[string]ThreadMeta
	repoKey       string
}

func SearchMemory(_ string, _ MemorySearchOptions) (MemorySearchResult, error) {
	return MemorySearchResult{Hits: []MemoryHit{}, Warnings: []string{}}, nil
}
