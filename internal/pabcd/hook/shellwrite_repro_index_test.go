package hook

import (
	"os"
	"strings"
	"testing"
)

// TestReproductionIndexHasRows fails when an indexed reproduction has no row, or a row has no index line. The index is the
// reviewed list of reproductions; a reproduction that cannot be a row is recorded in docs/port-cxc/known-defects/CRW-1028.md.
func TestReproductionIndexHasRows(t *testing.T) {
	b, err := os.ReadFile("testdata/shellir/reproductions.tsv")
	if err != nil {
		t.Fatal(err)
	}
	indexed := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("index line has %d fields, want 3: %q", len(parts), line)
		}
		indexed[parts[2]] = true
	}
	rows := map[string]bool{}
	for _, row := range reproductionRows() {
		rows[row.id] = true
	}
	for id := range indexed {
		if !rows[id] {
			t.Errorf("indexed reproduction %s has no row", id)
		}
	}
	for id := range rows {
		if !indexed[id] {
			t.Errorf("row %s is not in the index", id)
		}
	}
}
