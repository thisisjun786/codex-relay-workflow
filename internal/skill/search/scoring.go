package search

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/recall"
)

const (
	weightIDExact     = 10
	weightIDPart      = 5
	weightName        = 4
	weightCategory    = 3
	weightDescription = 2
)

func Tokenize(query string) []string {
	return strings.FieldsFunc(recall.Lower(query), func(r rune) bool { return r == ',' || r == '/' || text.Trim(string(r)) == "" })
}
func optional(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func ScoreRow(row SkillRow, terms []string) float64 {
	id, name := recall.Lower(row.ID), recall.Lower(row.Name)
	desc, ko, category := recall.Lower(row.Description), recall.Lower(optional(row.DescriptionKo)), recall.Lower(optional(row.Category))
	score := float64(0)
	for _, term := range terms {
		if id == term {
			score += weightIDExact
		} else if strings.Contains(id, term) {
			score += weightIDPart
		}
		if strings.Contains(name, term) {
			score += weightName
		}
		if strings.Contains(category, term) {
			score += weightCategory
		}
		if strings.Contains(desc, term) {
			score += weightDescription
		}
		if strings.Contains(ko, term) {
			score += weightDescription
		}
	}
	if score > 0 && (optional(row.SupersededBy) != "" || optional(row.Status) == "claude-specific") {
		score *= 0.5
	}
	return score
}

// Rank preserves input rows. Negative limits are slice(0, limit), counting from
// the end. Ties use the base's ASCII ICU-root order; Unicode ties remain a limit.
func Rank(rows []SkillRow, query string, limit int) []ScoredRow {
	out := []ScoredRow{}
	terms := Tokenize(query)
	if len(terms) == 0 {
		return out
	}
	for _, row := range rows {
		if score := ScoreRow(row, terms); score > 0 {
			out = append(out, ScoredRow{SkillRow: row, Score: score})
		}
	}
	slices.SortStableFunc(out, func(a, b ScoredRow) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return metric.CompareTimestamps(a.ID, b.ID)
	})
	if limit < 0 {
		limit = max(0, len(out)+limit)
	}
	return out[:min(limit, len(out))]
}
