// CXC v0.2.40 (3c1459ac) recall/src/index-search.ts:35-143,154-271.
// Query conditions only; ranking and queryIndex consume this library later.
package recall

import (
	"math"
	"strings"
	"unicode/utf8"
)

// ChatOrder selects recent ordering; other values, including empty, mean relevance.
type ChatOrder string

const (
	OrderRelevance ChatOrder = "relevance"
	OrderRecent    ChatOrder = "recent"
	// IndexSourceAll is a query sentinel, not a recorded rollout source.
	IndexSourceAll       RolloutSource = "all"
	maxRepoThreadIDs                   = 5000
	RRFK                               = 60
	LaneWeightFTS                      = 1.0
	LaneWeightTri                      = 0.8
	RecencyWeight                      = LaneWeightFTS / ((RRFK + 1) * (RRFK + 2))
	RecencyHalfLifeHours               = 24 * 7
	relaxedPool                        = 2000
	maxOptionalLaneWords               = 6
)

// IndexQueryOptions carries the upstream query contract. Empty strings represent
// null where the oracle checks truthiness; NowMs preserves a zero clock override.
// Numeric options retain JS Number semantics rather than truncating to integers.
type IndexQueryOptions struct {
	Plan             MatchPlan     `json:"plan"`
	Limit            float64       `json:"limit"`
	ContextN         float64       `json:"contextN"`
	CutoffISO        string        `json:"cutoffIso"`
	Role             string        `json:"role"`
	Cwd              string        `json:"cwd"`
	Source           RolloutSource `json:"source"`
	IncludeSynthetic bool          `json:"includeSynthetic"`
	IncludeTools     bool          `json:"includeTools"`
	Home             string        `json:"home"`
	Order            ChatOrder     `json:"order,omitempty"`
	RepoKey          string        `json:"repoKey"`
	NowMs            *float64      `json:"nowMs,omitempty"`
}

// resolvedQuery adds facts resolved by the caller against the open index/state DB.
type resolvedQuery struct {
	IndexQueryOptions
	repoThreadIDs    []string
	hasRepoKeyColumn bool
}

func poolSize(limit float64) float64 {
	return math.Min(500, math.Max(100, limit*10))
}

func planPoolSize(plan MatchPlan, limit float64) float64 {
	base := poolSize(limit)
	if !plan.AnyMode && len(plan.Required) == 0 && len(plan.Optional) > 0 {
		return math.Max(base, relaxedPool)
	}
	return base
}

// Lanes rank group leads only; they are not the authoritative match predicate.
func laneQuery(plan MatchPlan) (words []string, anyMode bool) {
	words = []string{}
	groups := plan.Optional
	anyMode = true
	if plan.AnyMode {
		groups = AllGroups(plan)
	} else if len(plan.Required) > 0 {
		groups, anyMode = plan.Required, false
	}
	optionalOnly := !plan.AnyMode && len(plan.Required) == 0
	for _, group := range groups {
		if len(group) == 0 || group[0].Text == "" {
			continue
		}
		word := group[0].Text
		if optionalOnly && utf8.RuneCountInString(word) < 3 {
			continue
		}
		words = append(words, word)
		if optionalOnly && len(words) >= maxOptionalLaneWords {
			break
		}
	}
	return words, anyMode
}

func ftsQuote(word string) string {
	return `"` + strings.ReplaceAll(word, `"`, `""`) + `"`
}

func escapeLike(word string) string {
	word = strings.ReplaceAll(word, `\`, `\\`)
	word = strings.ReplaceAll(word, `%`, `\%`)
	return strings.ReplaceAll(word, `_`, `\_`)
}

func sameOriginThreadIDs(meta ThreadMetaResult, repoKey string) []string {
	ids := []string{}
	if repoKey == "" {
		return ids
	}
	for _, id := range meta.IDs {
		origin := ""
		if thread := meta.ByID[id]; thread.GitOriginURL != nil {
			origin = *thread.GitOriginURL
		}
		if repoKeysEqual(repoKey, normalizeRepoKey(origin)) {
			ids = append(ids, id)
			if len(ids) >= maxRepoThreadIDs {
				break
			}
		}
	}
	return ids
}

func wordCondition(word string, params *[]any) string {
	if utf8.RuneCountInString(word) >= 3 {
		*params = append(*params, ftsQuote(word))
		return "m.id IN (SELECT rowid FROM msgs_tri WHERE msgs_tri MATCH ?)"
	}
	*params = append(*params, "%"+escapeLike(word)+"%")
	return `lower(m.text) LIKE ? ESCAPE '\'`
}

func groupCondition(group QueryGroup, params *[]any) string {
	if len(group) == 0 {
		return "1"
	}
	conditions := make([]string, 0, len(group))
	for _, term := range group {
		conditions = append(conditions, wordCondition(term.Text, params))
	}
	return "(" + strings.Join(conditions, " OR ") + ")"
}

func candidateFilter(opts resolvedQuery, withWords bool) (string, []any) {
	return candidateFilterFor(opts, withWords, FoldCwdCase())
}

// fold is an explicit platform seam; production callers use candidateFilter.
// Only fixed SQL fragments and placeholder counts enter SQL. All values are bound.
func candidateFilterFor(opts resolvedQuery, withWords, fold bool) (string, []any) {
	params, conditions := []any{}, []string{}
	if withWords {
		groups, joiner := opts.Plan.Required, " AND "
		if opts.Plan.AnyMode {
			groups, joiner = AllGroups(opts.Plan), " OR "
		}
		if len(groups) > 0 {
			parts := make([]string, 0, len(groups))
			for _, group := range groups {
				parts = append(parts, groupCondition(group, &params))
			}
			conditions = append(conditions, "("+strings.Join(parts, joiner)+")")
		}
	}
	if !opts.IncludeSynthetic {
		conditions = append(conditions, "m.synthetic = 0")
	}
	if !opts.IncludeTools {
		conditions = append(conditions, "m.match_field = 'content'")
	}
	if opts.Role != "" {
		conditions = append(conditions, "m.role = ?")
		params = append(params, opts.Role)
	}
	if opts.CutoffISO != "" {
		conditions = append(conditions, "m.ts >= ?")
		params = append(params, opts.CutoffISO)
	}
	if opts.Source != IndexSourceAll {
		conditions = append(conditions, "f.source = ?")
		params = append(params, string(opts.Source))
	}
	if opts.Cwd != "" {
		cwd, column := NormalizeCwd(opts.Cwd), CanonicalCwdSQL("f.cwd")
		eq, like := column+" = ?", column+` LIKE ? ESCAPE '\'`
		prefix := cwd
		if fold {
			eq, like = "lower("+column+") = lower(?)", "lower("+column+`) LIKE ? ESCAPE '\'`
			prefix = Lower(cwd)
		}
		parts := []string{eq, like}
		params = append(params, cwd, escapeLike(prefix)+"/%")
		if opts.RepoKey != "" && opts.hasRepoKeyColumn {
			parts = append(parts, "(f.repo_key IS NOT NULL AND f.repo_key = ?)")
			params = append(params, opts.RepoKey)
		}
		if len(opts.repoThreadIDs) > 0 {
			parts = append(parts, "f.thread_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(opts.repoThreadIDs)), ",")+")")
			for _, id := range opts.repoThreadIDs {
				params = append(params, id)
			}
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}
	if len(conditions) == 0 {
		return "1", params
	}
	return strings.Join(conditions, " AND "), params
}

func textMatches(text string, plan MatchPlan) bool {
	return PlanMatches(Lower(text), plan)
}
