// CXC v0.2.40 (3c1459ac) recall/src/index-search.ts:35-143,154-271.
// Query conditions only; ranking and queryIndex consume this library later.
package recall

import (
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	indexMaxRepoThreadIDs = 5000
	RRFK                  = 60
	LaneWeightFTS         = 1.0
	LaneWeightTri         = 0.8
	RecencyWeight         = LaneWeightFTS / ((RRFK + 1) * (RRFK + 2))
	RecencyHalfLifeHours  = 24 * 7
	relaxedPool           = 2000
	maxOptionalLaneWords  = 6
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
	// ChatRecent selects recency; empty or other order values mean ChatRelevance.
	Order   ChatOrder `json:"order,omitempty"`
	RepoKey string    `json:"repoKey"`
	NowMs   *float64  `json:"nowMs,omitempty"`
}

// resolvedQuery adds facts resolved by the caller against the open index/state DB.
type resolvedQuery struct {
	IndexQueryOptions
	repoThreadIDs    []string
	hasRepoKeyColumn bool
	// foldedCwds, when foldedResolved, is every stored cwd the scan's own predicate (CwdMatches, folded) accepts for Cwd. SQL's lower()
	// folds ASCII only, so a case-insensitive host asks the same Go predicate that the scan asks (known-defects.md :662).
	foldedCwds     []string
	foldedResolved bool
}

// maxFoldedCwds bounds the stored cwd values bound into one statement; a larger set keeps the SQL predicate.
const maxFoldedCwds = 10000

// resolveFoldedCwds lists the distinct stored cwd values that CwdMatches accepts for cwd under case folding.
func resolveFoldedCwds(db *RwDb, cwd string) ([]string, bool) {
	rows, err := indexRankRead(db, "SELECT DISTINCT cwd FROM files WHERE cwd IS NOT NULL")
	if err != nil {
		return nil, false
	}
	out := []string{}
	for _, row := range rows {
		if stored, ok := row["cwd"].(string); ok && CwdMatches(stored, cwd, true) {
			if out = append(out, stored); len(out) > maxFoldedCwds {
				return nil, false
			}
		}
	}
	return out, true
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

func indexSameOriginThreadIDs(meta ThreadMetaResult, repoKey string) []string {
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
			if len(ids) >= indexMaxRepoThreadIDs {
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
	like := `lower(m.text) LIKE ? ESCAPE '\'`
	// SQLite folds ASCII only, while the final predicate lowers the whole text: a short word is also looked up as each spelling of
	// its letters that SQL cannot fold (Ü for ü, the Kelvin sign for k, İ for i), so no row the final predicate accepts is cut off
	// before it (known-defects.md :661).
	extra := shortWordSpellings(word)
	if len(extra) == 0 {
		return like
	}
	conditions := []string{like}
	for _, spelling := range extra {
		conditions = append(conditions, "instr(m.text, ?) > 0")
		*params = append(*params, spelling)
	}
	return "(" + strings.Join(conditions, " OR ") + ")"
}

var (
	caseSourcesOnce sync.Once
	caseSources     map[rune][]rune
)

// lowerSources are the runes, other than r itself, whose lowercase holds r.
func lowerSources(r rune) []rune {
	caseSourcesOnce.Do(func() {
		caseSources = map[rune][]rune{}
		for _, cr := range unicode.CaseRanges {
			for c := rune(cr.Lo); c <= rune(cr.Hi); c++ {
				if l := unicode.ToLower(c); l != c {
					caseSources[l] = append(caseSources[l], c)
				}
			}
		}
		caseSources['i'] = append(caseSources['i'], 0x130) // İ lowers to i and a combining dot above
		caseSources[0x307] = append(caseSources[0x307], 0x130)
		caseSources[0x3c2] = append(caseSources[0x3c2], 0x3a3) // a capital sigma ending a word lowers to the final form
	})
	return caseSources[r]
}

// wordSpellings are the spellings of a word, other than itself, whose text lowers (with Lower) to a string holding the word, that have
// a letter outside ASCII, and that SQLite's ASCII fold does not reach: Ü for ü, the Kelvin sign for k, İ for i. withASCIICase also
// lists the upper-case ASCII spellings, for a case-sensitive lookup. ok is false when there would be more than limit of them.
func wordSpellings(word string, withASCIICase bool, limit int) (out []string, ok bool) {
	runes := []rune(word)
	options := make([][]rune, len(runes))
	total := 1
	for i, r := range runes {
		options[i] = []rune{r}
		for _, source := range lowerSources(r) {
			if source >= 0x80 || withASCIICase {
				options[i] = append(options[i], source)
			}
		}
		if total *= len(options[i]); total > limit {
			return nil, false
		}
	}
	spellings := []string{""}
	for _, choices := range options {
		next := make([]string, 0, len(spellings)*len(choices))
		for _, prefix := range spellings {
			for _, c := range choices {
				next = append(next, prefix+string(c))
			}
		}
		spellings = next
	}
	if len(runes) == 2 && Lower("\u0130") == word {
		spellings = append(spellings, "\u0130")
	}
	for _, spelling := range spellings {
		if asciiLower(spelling) == word || slices.Contains(out, spelling) {
			continue // the LIKE reaches it, or it is listed
		}
		if strings.IndexFunc(spelling, func(r rune) bool { return r >= 0x80 }) >= 0 {
			out = append(out, spelling)
		}
	}
	return out, true
}

// shortWordSpellings are the case-sensitive spellings to look up beside the ASCII LIKE for a word of one or two letters.
func shortWordSpellings(word string) []string {
	if n := utf8.RuneCountInString(word); n == 0 || n > 2 {
		return nil
	}
	out, _ := wordSpellings(word, true, 64)
	return out
}

// asciiLower folds A-Z only, as SQLite's lower() does.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
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
	if opts.Source != RolloutAll {
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
		if fold && opts.foldedResolved {
			// SQL folds ASCII only: the stored cwd values the scan's own predicate accepts stand in for the two comparisons.
			parts, params = parts[:0], params[:len(params)-2]
			if len(opts.foldedCwds) > 0 {
				parts = append(parts, "f.cwd IN ("+strings.TrimSuffix(strings.Repeat("?,", len(opts.foldedCwds)), ",")+")")
				for _, stored := range opts.foldedCwds {
					params = append(params, stored)
				}
			}
		}
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
		if len(parts) == 0 {
			parts = append(parts, "0") // no stored cwd matches, and no repository does either
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

// planHasNUL reports whether a query term holds a NUL character: FTS5 quoting cannot carry one (MATCH raises "unterminated string"),
// so such a query is served by the scan (known-defects.md :663).
func planHasNUL(plan MatchPlan) bool {
	for _, group := range AllGroups(plan) {
		for _, term := range group {
			if strings.ContainsRune(term.Text, 0) {
				return true
			}
		}
	}
	return false
}

var errIndexNULWord = errors.New("a query word holds a NUL character, which the index cannot search")
