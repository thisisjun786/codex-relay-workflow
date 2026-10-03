package recall

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRemoteURLRecordedGrids(t *testing.T) {
	b, err := os.ReadFile("testdata/repokey/oracle-grid.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Group, Raw           string
		Key                  *string
		Port, Classification string
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	for _, row := range rows {
		groups[row.Group]++
		want := ""
		if row.Key != nil {
			want = *row.Key
		}
		if row.Classification == "platform-difference" {
			if want == row.Port {
				t.Fatalf("%s no longer demonstrates its declared platform difference", row.Raw)
			}
			want = row.Port
		}
		if got := normalizeRepoKey(row.Raw); got != want {
			t.Errorf("%s %q: got %q, want %q (Node %v)", row.Group, row.Raw, got, want, row.Key)
		}
	}
	for _, group := range []string{"ipv4", "ipv6", "punycode", "states", "nfc", "compatibility", "disallowed", "bidi", "contextj", "ace-validation"} {
		if groups[group] == 0 {
			t.Errorf("missing grid %s", group)
		}
	}
	t.Logf("replayed %d recorded rows in %d grids", len(rows), len(groups))
}
