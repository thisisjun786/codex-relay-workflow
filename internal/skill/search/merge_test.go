package search

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mergeFetch serves one jaw registry, one hermes catalog and one ClawHub answer; rows are written as in the
// catalogs themselves.
func mergeFetch(jaw string, hermes []string, clawhub []string) FetchText {
	return func(url string) (string, error) {
		switch {
		case url == JAWRegistryURL:
			return jaw, nil
		case url == HermesCatalogURL:
			lines := []string{}
			for _, id := range hermes {
				lines = append(lines, fmt.Sprintf("| [`%s`](x) | %s | `development/%s` |", strings.SplitN(id, "=", 2)[0], strings.SplitN(id, "=", 2)[1], strings.SplitN(id, "=", 2)[0]))
			}
			return strings.Join(lines, "\n"), nil
		case strings.Contains(url, "/search?"):
			results := []string{}
			for _, slug := range clawhub {
				results = append(results, fmt.Sprintf(`{"slug":%q,"summary":"nothing relevant"}`, slug))
			}
			return `{"results":[` + strings.Join(results, ",") + `]}`, nil
		}
		return "body", nil
	}
}

func mergedRows(t *testing.T, args []string, fetch FetchText) []ScoredRow {
	t.Helper()
	code, out, errOut := cliRun(append([]string{"search"}, append(args, "--json")...), fetch)
	var rows []ScoredRow
	if code != 0 || errOut != "" || json.Unmarshal([]byte(out), &rows) != nil {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	return rows
}

func keys(rows []ScoredRow) string {
	parts := []string{}
	for _, r := range rows {
		parts = append(parts, string(r.Source)+":"+r.ID)
	}
	return strings.Join(parts, " ")
}

func others(n int) []string {
	out := []string{}
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("other%d", i))
	}
	return out
}

func TestExactMatchOutranksAnotherSourcesRankOneWhateverItsRowCount(t *testing.T) {
	const jaw = `{"skills":{"telegram-send":{"name":"Telegram Send","description":"send telegram messages"}}}`
	for _, n := range []int{1, 20} {
		t.Run(fmt.Sprint(n, " clawhub rows"), func(t *testing.T) {
			cliHome(t)
			rows := mergedRows(t, []string{"telegram-send", "--source", "all", "--limit", "1"}, mergeFetch(jaw, nil, others(n)))
			if len(rows) != 1 || rows[0].Source != SourceJaw || rows[0].ID != "telegram-send" {
				t.Fatalf("top-1 = %s", keys(rows))
			}
		})
	}
}

func TestExactNameOutranksNativeRankInAnotherRow(t *testing.T) {
	cliHome(t)
	const jaw = `{"skills":{"telegram-send-pro":{"name":"Pro","description":"telegram send"},"ts":{"name":"Telegram Send","description":"x"}}}`
	rows := mergedRows(t, []string{"Telegram Send", "--source", "all", "--limit", "1"}, mergeFetch(jaw, []string{"hermes-telegram=telegram send tool"}, others(3)))
	if len(rows) != 1 || rows[0].ID != "ts" {
		t.Fatalf("top-1 = %s", keys(rows))
	}
}

func TestMergeKeepsEachSourcesNativeOrderAndInterleavesByRank(t *testing.T) {
	cliHome(t)
	const jaw = `{"skills":{"alpha-one":{"name":"Alpha One","description":"alpha"},"alpha-two":{"name":"A2","description":"alpha"}}}`
	hermes := []string{"hermes-alpha-one=alpha", "hermes-alpha-two=alpha"}
	rows := mergedRows(t, []string{"alpha", "--source", "all", "--limit", "10"}, mergeFetch(jaw, hermes, nil))
	// jaw's two rows both score above hermes's two natively; the merge interleaves by rank in the source, not by
	// the size of the source's own score.
	want := "jaw:alpha-one hermes:hermes-alpha-one jaw:alpha-two hermes:hermes-alpha-two"
	if keys(rows) != want {
		t.Fatalf("%s\nwant %s", keys(rows), want)
	}
	if rows[0].Score != 11 || rows[2].Score != 7 {
		t.Fatalf("score stays the score of the row's own source: %+v", rows)
	}
}

func TestSameIDInDifferentSourcesStaysDistinctAndDeterministic(t *testing.T) {
	const jaw = `{"skills":{"tdd":{"name":"TDD","description":"loop"}}}`
	for i := 0; i < 25; i++ {
		cliHome(t)
		rows := mergedRows(t, []string{"tdd", "--source", "all"}, mergeFetch(jaw, []string{"tdd=test driven"}, []string{"tdd", "other"}))
		if got := keys(rows[:3]); got != "jaw:tdd hermes:tdd clawhub:tdd" {
			t.Fatalf("run %d: %s", i, keys(rows))
		}
	}
}

func TestMergeWithAFailedSourceKeepsTheOthersInOrder(t *testing.T) {
	cliHome(t)
	const jaw = `{"skills":{"alpha-one":{"name":"A1","description":"alpha"}}}`
	fetch := func(url string) (string, error) {
		if url == HermesCatalogURL {
			return "", fmt.Errorf("down")
		}
		return mergeFetch(jaw, nil, []string{"alpha-clawhub"})(url)
	}
	code, out, errOut := cliRun([]string{"search", "alpha", "--source", "all", "--json"}, fetch)
	var rows []ScoredRow
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || keys(rows) != "jaw:alpha-one clawhub:alpha-clawhub" || errOut != "skill-search: source hermes failed (down)\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

// A search of one named source has nothing to merge: its rows keep the source's native order, with no fusion and no
// exact-match boost, and only the limit applies. An exact id lower in ClawHub's answer, or a superseded row whose id
// matches the query exactly in jaw, does not take the native rank-1 row's place.
func TestSingleSourceSearchKeepsTheNativeOrder(t *testing.T) {
	t.Run("clawhub later exact id", func(t *testing.T) {
		cliHome(t)
		rows := mergedRows(t, []string{"tdd", "--source", "clawhub", "--limit", "1"}, mergeFetch("", nil, []string{"other0", "tdd"}))
		if keys(rows) != "clawhub:other0" {
			t.Fatalf("top-1 = %s", keys(rows))
		}
		cliHome(t)
		rows = mergedRows(t, []string{"tdd", "--source", "clawhub"}, mergeFetch("", nil, []string{"other0", "tdd"}))
		if keys(rows) != "clawhub:other0 clawhub:tdd" {
			t.Fatalf("rows = %s", keys(rows))
		}
	})
	t.Run("jaw superseded exact id", func(t *testing.T) {
		const jaw = `{"skills":{"tdd":{"name":"TDD","description":"","superseded_by":"tdd-active"},"tdd-active":{"name":"TDD active","description":""}}}`
		native, err := FetchJawRows(func(string) (string, error) { return jaw, nil })
		if err != nil {
			t.Fatal(err)
		}
		ranked := Rank(native, "tdd", len(native))
		if len(ranked) != 2 || ranked[0].ID != "tdd-active" || ranked[0].Score <= ranked[1].Score {
			t.Fatalf("native rank: %+v", ranked)
		}
		cliHome(t)
		rows := mergedRows(t, []string{"tdd", "--source", "jaw", "--limit", "1"}, mergeFetch(jaw, nil, nil))
		if keys(rows) != "jaw:tdd-active" || rows[0].Score != ranked[0].Score {
			t.Fatalf("top-1 = %s %+v", keys(rows), rows)
		}
	})
}

// The limit applies to the merged list, never to a source's list before the merge: an exact match that is not the
// native rank 1 of its source (here ClawHub's second row, the other sources having nothing) is still the top-1 of a
// search over all sources.
func TestMultiSourceTopOneIsFoundBeyondTheNativeRankOne(t *testing.T) {
	cliHome(t)
	rows := mergedRows(t, []string{"tdd", "--source", "all", "--limit", "1"}, mergeFetch(`{"skills":{}}`, nil, []string{"other0", "tdd"}))
	if keys(rows) != "clawhub:tdd" {
		t.Fatalf("top-1 = %s", keys(rows))
	}
	cliHome(t)
	const jaw = `{"skills":{"tdd-active":{"name":"Active","description":"tdd tdd tdd tdd"},"tdd-old":{"name":"Old","description":"tdd"}}}`
	rows = mergedRows(t, []string{"tdd", "--source", "all", "--limit", "1"}, mergeFetch(jaw, nil, []string{"other0", "tdd"}))
	if keys(rows) != "clawhub:tdd" {
		t.Fatalf("top-1 = %s", keys(rows))
	}
}
