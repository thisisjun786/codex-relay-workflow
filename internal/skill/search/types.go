// Package search ports CXC v0.2.40 skill-search (commit 3c1459ac), under the CRW
// name table. Fetches are injected; catalog bodies are untrusted bounded data.
// Ranking uses the existing ASCII ICU-root comparator. Non-ASCII ties and the
// oracle's host-dependent locale are not reproduced (known-defects.md).
package search

// Source identifies the catalog that owns a row; gh is populated by the caller.
type Source string

const (
	SourceJaw     Source = "jaw"
	SourceHermes  Source = "hermes"
	SourceClawhub Source = "clawhub"
	SourceGH      Source = "gh"
)

// Requires keeps absent arrays distinct from present empty arrays.
type Requires struct {
	Bins   []string `json:"bins,omitzero"`
	Env    []string `json:"env,omitzero"`
	System []string `json:"system,omitzero"`
}

// SkillRow is the common unit emitted by a remote catalog adapter.
type SkillRow struct {
	ID            string    `json:"id"`
	Source        Source    `json:"source"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	DescriptionKo *string   `json:"descriptionKo,omitempty"`
	Category      *string   `json:"category,omitempty"`
	RawURL        string    `json:"rawUrl"`
	SupersededBy  *string   `json:"supersededBy,omitempty"`
	Status        *string   `json:"status,omitempty"`
	Requires      *Requires `json:"requires,omitempty"`
}

// ScoredRow adds the additive keyword score to a row.
type ScoredRow struct {
	SkillRow
	Score float64 `json:"score"`
}

// FetchText reads one URL. The caller owns transport timeouts and read limits.
type FetchText func(url string) (string, error)
