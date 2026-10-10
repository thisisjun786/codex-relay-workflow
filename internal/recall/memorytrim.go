package recall

import (
	"math"
	"slices"
	"unicode/utf16"
)

// RankAndTrim ports CXC v0.2.40 recall/src/memory-search.ts:221-236 with the CRW-1128 repairs. Candidates are sorted in place by score
// (a NaN score ranks last), then by date (newer first), then by a stable identity (path, start line, origin, kind), so equal hits
// compare as equal and the order does not depend on the input order or the sort's schedule (known-defects.md :694, :695). Each
// Relpath is capped, and the limit is checked before a hit is added: a limit below one returns nothing, a fraction is whole hits, and
// NaN and +Inf do not limit (known-defects.md :696).
func RankAndTrim(candidates []MemoryHit, limit float64) []MemoryHit {
	JSSort(candidates, func(a, b MemoryHit) float64 { return float64(compareMemoryHits(a, b)) })
	if !math.IsNaN(limit) {
		limit = math.Floor(limit)
	}
	perFile, out := map[string]int{}, []MemoryHit{}
	for _, hit := range candidates {
		if limit < 1 { // false for NaN
			break
		}
		if float64(len(out)) >= limit {
			break
		}
		n := perFile[hit.Relpath]
		if n >= perFileCap {
			continue
		}
		perFile[hit.Relpath] = n + 1
		out = append(out, hit)
	}
	return out
}

// compareMemoryHits is the ranking order: negative when a ranks before b.
func compareMemoryHits(a, b MemoryHit) int {
	sa, sb := memoryRankScore(a.Score), memoryRankScore(b.Score)
	if sa != sb {
		if sa > sb {
			return -1
		}
		return 1
	}
	text := func(p *string) []uint16 {
		if p == nil {
			return nil
		}
		return utf16.Encode([]rune(*p))
	}
	if c := slices.Compare(text(a.UpdatedAt), text(b.UpdatedAt)); c != 0 {
		return -c
	}
	if c := slices.Compare(utf16.Encode([]rune(a.Relpath)), utf16.Encode([]rune(b.Relpath))); c != 0 {
		return c
	}
	line := func(p *int) int {
		if p == nil {
			return math.MaxInt
		}
		return *p
	}
	if la, lb := line(a.StartLine), line(b.StartLine); la != lb {
		if la < lb {
			return -1
		}
		return 1
	}
	if a.Origin != b.Origin {
		if a.Origin < b.Origin {
			return -1
		}
		return 1
	}
	if a.Kind != b.Kind {
		if a.Kind < b.Kind {
			return -1
		}
		return 1
	}
	return 0
}

// memoryRankScore puts a NaN score below every number.
func memoryRankScore(score float64) float64 {
	if math.IsNaN(score) {
		return math.Inf(-1)
	}
	return score
}
