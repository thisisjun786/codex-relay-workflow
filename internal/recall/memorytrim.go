package recall

import (
	"slices"
	"unicode/utf16"
)

// RankAndTrim ports CXC v0.2.40 recall/src/memory-search.ts:221-236.
// It sorts candidates in place, caps each Relpath, then tests the numeric limit
// after appending. Thus nonpositive limits keep the first hit; NaN and +Inf
// never break. Equal score/date pairs deliberately compare as -1 both ways.
func RankAndTrim(candidates []MemoryHit, limit float64) []MemoryHit {
	JSSort(candidates, func(a, b MemoryHit) float64 {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		date := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		if slices.Compare(utf16.Encode([]rune(date(a.UpdatedAt))), utf16.Encode([]rune(date(b.UpdatedAt)))) < 0 {
			return 1
		}
		return -1
	})
	perFile, out := map[string]int{}, []MemoryHit{}
	for _, hit := range candidates {
		n := perFile[hit.Relpath]
		if n >= perFileCap {
			continue
		}
		perFile[hit.Relpath] = n + 1
		out = append(out, hit)
		if float64(len(out)) >= limit {
			break
		}
	}
	return out
}
