package recall

import (
	"slices"
	"strconv"
	"testing"
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

func TestRankAndTrimNodeOracle(t *testing.T) {
	for _, c := range jsSortReadOracle(t).Ranks {
		t.Run(c.Name, func(t *testing.T) {
			candidates := memoryTrimGenerate(t, c)
			out := RankAndTrim(candidates, jsSortNumber(t, c.Limit))
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
