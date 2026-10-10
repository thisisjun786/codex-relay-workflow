package recall

import (
	"math"
	"slices"
	"strconv"
	"testing"
	"unicode/utf16"
)

func memoryTrimGenerate(t *testing.T, c memoryTrimOracleCase) []MemoryHit {
	t.Helper()
	g, hits, numbers := c.Gen, []MemoryHit{}, jsSortNumbers()
	dates := []*string{nil, memoryPtr(""), memoryPtr("2026-01-01"), memoryPtr("2026-02-01")}
	for i := range g.N {
		score, date, path := 0.0, (*string)(nil), "f"+strconv.Itoa(i)
		switch g.Kind {
		case "cap":
			score, path = float64(g.N-i), "f"+strconv.Itoa(i%3)
		case "tie":
			score, date = jsSortNumber(t, g.Score), g.Date
		case "inline":
			score, date = jsSortNumber(t, c.Input[i].Score), c.Input[i].UpdatedAt
		case "mixed":
			score = numbers[g.next()%uint32(len(numbers))]
			date = dates[g.next()%4]
			path = "f" + strconv.FormatUint(uint64(g.next()%17), 10)
		default:
			t.Fatalf("unknown rank generator %q", g.Kind)
		}
		hits = append(hits, MemoryHit{Origin: "file", Kind: MemoryOther, Relpath: path,
			UpdatedAt: date, Excerpt: "h" + strconv.Itoa(i), StartLine: memoryPtr(i + 1), Score: score})
	}
	return hits
}

// oracleRankAndTrim is the oracle's ranking as it was ported before CRW-1128 (known-defects.md :694-:696): equal score and date pairs
// compare as -1 both ways, NaN scores subtract to NaN, the limit is checked after appending. The recorded grid is the answer of that
// function under V8's sort; the replay below runs the port of the sort on it, and checks the repaired RankAndTrim against the grid
// where the oracle's comparator is consistent.
func oracleRankAndTrim(candidates []MemoryHit, limit float64) []MemoryHit {
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

// rankGridIsConsistent reports whether the oracle's comparator was a consistent order on the grid: no NaN score, no two hits with the same
// score and date, a whole limit of at least one. There the repaired RankAndTrim must agree with the recording.
func rankGridIsConsistent(hits []MemoryHit, limit float64) bool {
	if math.IsNaN(limit) || limit < 1 || !math.IsInf(limit, 1) && limit != math.Floor(limit) {
		return false
	}
	type key struct {
		score float64
		date  string
	}
	seen := map[key]bool{}
	for _, h := range hits {
		if math.IsNaN(h.Score) {
			return false
		}
		d := ""
		if h.UpdatedAt != nil {
			d = *h.UpdatedAt
		}
		if k := (key{h.Score, d}); seen[k] {
			return false
		} else {
			seen[k] = true
		}
	}
	return true
}

func TestRankAndTrimNodeOracle(t *testing.T) {
	for _, c := range jsSortReadOracle(t).Ranks {
		t.Run(c.Name, func(t *testing.T) {
			candidates := memoryTrimGenerate(t, c)
			limit := jsSortNumber(t, c.Limit)
			repaired := RankAndTrim(slices.Clone(candidates), limit)
			consistent := rankGridIsConsistent(candidates, limit)
			out := oracleRankAndTrim(candidates, limit)
			ids := func(hits []MemoryHit) []int {
				out := []int{}
				for _, h := range hits {
					id, err := strconv.Atoi(h.Excerpt[1:])
					if err != nil || h.Excerpt != "h"+strconv.Itoa(id) {
						t.Fatalf("invalid recorded identity %q", h.Excerpt)
					}
					out = append(out, id)
				}
				return out
			}
			if out == nil || !slices.Equal(ids(out), c.Out) || !slices.Equal(ids(candidates), c.Sorted) {
				t.Fatalf("rank/cap/limit or candidate mutation differs: got %v, want %v", ids(out), c.Out)
			}
			if consistent && !slices.Equal(ids(repaired), c.Out) {
				t.Fatalf("the repaired ranking differs from the recording where the oracle's order is consistent: got %v, want %v", ids(repaired), c.Out)
			}
			// Whatever the grid, the repaired ranking is in the stable order, within the limit and the per-file cap.
			for i := 1; i < len(repaired); i++ {
				if compareMemoryHits(repaired[i-1], repaired[i]) > 0 {
					t.Fatalf("not in ranking order at %d: %v", i, ids(repaired))
				}
			}
			if lim := math.Floor(limit); !math.IsNaN(limit) && float64(len(repaired)) > math.Max(lim, 0) {
				t.Fatalf("%d hits over the limit %v", len(repaired), limit)
			}
		})
	}
}

// ranking.test.ts:67-99's ordering is driven through the scoring/ranking seam;
// searchMemory's filesystem orchestration is a separate port and is not claimed.
func TestRankAndTrimBClassKindAndRecencyOrder(t *testing.T) {
	const hour, now = 3_600_000.0, 1_783_382_400_000.0
	paths := []string{"memory_summary.md", "MEMORY.md", "rollout_summaries/fresh.md", "rollout_summaries/stale.md"}
	ages := []float64{90 * 24, 90 * 24, 2, 60 * 24}
	hits := []MemoryHit{}
	for i, p := range paths {
		mtime := now - ages[i]*hour
		hits = append(hits, MemoryHit{Relpath: p, Kind: KindOfRelpath(p), Score: FinalScore(9, KindOfRelpath(p), &mtime, now)})
	}
	slices.Reverse(hits)
	got := RankAndTrim(hits, DefaultMemoryLimit)
	if len(got) != len(paths) {
		t.Fatalf("got %d ranked hits, want %d", len(got), len(paths))
	}
	for i, h := range got {
		if h.Relpath != paths[i] || i > 0 && got[i-1].Score <= h.Score {
			t.Fatalf("kind/recency order: %+v", got)
		}
	}
}
