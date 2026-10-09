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
