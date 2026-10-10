package search

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/recall"
)

// The merge policy of a search over several sources. The sources' scores do not share a scale: jaw and hermes add up
// keyword weights, ClawHub's is the number of rows it returned minus the row's position, gh's the same. A merged
// order that sorts on them lets the size of one source's answer decide the relevance of its rows, so the merge uses
// what every source does share, the order it ranked its own rows in (its native order, which is kept), as a weighted
// reciprocal-rank fusion, and then puts an exact match of the whole query on a skill's id, or on its name, first.
//
//	merged = sourceWeight / (mergeRankOffset + nativeRank) + boost       (nativeRank counts from 1)
//	boost  = mergeBoostID when lower(id) equals the query, else mergeBoostName when lower(name) does, else 0
//
// Both boosts are larger than any sum the fusion term can reach, so an exact id outranks an exact name and an exact
// name outranks every other row, whichever source it comes from. Rows from different sources are never merged into
// one, even with the same id: each keeps its source. A tie is broken by the order the sources were named in, then
// by the native order, so the result does not depend on which source answered first. `score` stays the score the
// row's own source gave it and is comparable only with rows of that source (ClawHub's and gh's carry no relevance).
// A search of one named source (--source jaw, hermes, clawhub or gh) is not merged: its rows keep the source's native
// order, with neither the fusion nor the boost, and only the limit applies; `all` is merged even when one source answers.
const (
	mergeRankOffset = 60
	mergeBoostID    = 1.0
	mergeBoostName  = 0.5
)

var mergeSourceWeight = map[Source]float64{SourceJaw: 1, SourceHermes: 1, SourceClawhub: 0.8, SourceGH: 0.6}

func normalizeQuery(s string) string { return strings.Join(strings.Fields(recall.Lower(s)), " ") }

func mergeBoost(row SkillRow, query string) float64 {
	switch {
	case query == "":
		return 0
	case normalizeQuery(row.ID) == query:
		return mergeBoostID
	case normalizeQuery(row.Name) == query:
		return mergeBoostName
	}
	return 0
}

// mergeSources merges the answers of the sources, each in its native order and in the order the sources were named.
func mergeSources(query string, lists [][]ScoredRow) []ScoredRow {
	type entry struct {
		row    ScoredRow
		merged float64
		list   int
		rank   int
	}
	q := normalizeQuery(query)
	entries := []entry{}
	for l, list := range lists {
		for i, row := range list {
			weight, ok := mergeSourceWeight[row.Source]
			if !ok {
				weight = 0.5
			}
			entries = append(entries, entry{row, weight/float64(mergeRankOffset+i+1) + mergeBoost(row.SkillRow, q), l, i})
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		switch {
		case a.merged > b.merged:
			return -1
		case a.merged < b.merged:
			return 1
		case a.list != b.list:
			return a.list - b.list
		}
		return a.rank - b.rank
	})
	out := make([]ScoredRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.row)
	}
	return out
}
