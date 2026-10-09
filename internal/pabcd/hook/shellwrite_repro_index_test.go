package hook

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// reproIndexLine is one line of testdata/shellir/reproductions.tsv: a reproduction of a source file and what stands for it.
type reproIndexLine struct{ source, text, row, kind, reason string }

func readReproIndex(t *testing.T) []reproIndexLine {
	t.Helper()
	b, err := os.ReadFile("testdata/shellir/reproductions.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var out []reproIndexLine
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			t.Fatalf("index line has %d fields, want 5: %q", len(f), line)
		}
		out = append(out, reproIndexLine{f[0], f[1], f[2], f[3], f[4]})
	}
	return out
}

// TestReproductionIndexHasRows fails when an indexed reproduction has neither a row nor a reason, when a row has no index line, or
// when a line names a row that does not exist. A line is of kind row (this reproduction is that row, once per row), same (the same
// command as that row) or prose (no command: a sentence, a record claim, a race; the row is the nearest command shape, or - when
// the source holds none). The index is the reviewed list of reproductions; what cannot be a row is named here with its reason.
func TestReproductionIndexHasRows(t *testing.T) {
	rows := map[string]bool{}
	for _, row := range reproductionRows() {
		rows[row.id] = true
	}
	owner := map[string]int{}
	for _, l := range readReproIndex(t) {
		if l.source == "" || l.text == "" {
			t.Errorf("index line with an empty source or text: %+v", l)
		}
		switch l.kind {
		case "row":
			if !rows[l.row] {
				t.Errorf("indexed reproduction %s has no row", l.row)
			}
			if l.reason != "-" {
				t.Errorf("row line %s carries a reason (%q); a row line has - in the reason field", l.row, l.reason)
			}
			owner[l.row]++
		case "same":
			if !rows[l.row] {
				t.Errorf("same-as line names row %s, which does not exist: %s", l.row, l.text)
			}
			if !strings.HasPrefix(l.reason, "same") {
				t.Errorf("same-as line %q needs a reason starting with same: %q", l.text, l.reason)
			}
		case "prose":
			if l.row != "-" && !rows[l.row] {
				t.Errorf("prose line names row %s, which does not exist: %s", l.row, l.text)
			}
			if !strings.HasPrefix(l.reason, "prose") {
				t.Errorf("prose line %q needs a reason starting with prose: %q", l.text, l.reason)
			}
		default:
			t.Errorf("unknown kind %q: %s", l.kind, l.text)
		}
	}
	for id := range rows {
		if owner[id] != 1 {
			t.Errorf("row %s has %d index lines of kind row, want 1", id, owner[id])
		}
	}
}

// reproExpectedSources are the files the reproductions come from: the issue texts of the reader family, the frozen evaluation
// records and the frozen handoffs. Every one of them must have at least one index line, so a source cannot be dropped unnoticed.
var reproExpectedSources = []string{
	"linear-issue-CRW-765.json", "linear-issue-CRW-851.json", "linear-issue-CRW-875.json", "linear-issue-CRW-894.json",
	"linear-issue-CRW-917.json", "linear-issue-CRW-941.json", "linear-issue-CRW-951.json", "linear-issue-CRW-984.json",
	"linear-issue-CRW-986.json", "linear-issue-CRW-989.json", "linear-issue-CRW-998.json", "linear-issue-CRW-1011.json",
	"linear-issue-CRW-1012.json", "linear-issue-CRW-1014.json",
	"linear-issue-CRW-1064.json",
	"frozen/pre-eval-records-bundle.json", "frozen/pair-eval-ledger-rows.json",
	"frozen/crw-765-handoff-562c59e1f97c.json", "frozen/crw-765-wip-20261008.patch", "frozen/crw-875-handoff-f30490c14470.json",
	"frozen/crw-894-handoff-g3-3113ad91f272.json", "frozen/crw-951-handoff-904053760577.json",
}

// TestReproductionCountsPerSource counts the reproductions per source file and kind and logs the table, so completeness can be
// checked against the inputs. Each expected source must have a line; the four single evaluation records (pre-eval-pr774-562c59e1,
// pr825-3113ad91, pr845-f30490c1, pr871-90405376) are copies of entries of the bundle and are named in the source of those lines.
func TestReproductionCountsPerSource(t *testing.T) {
	type count struct{ row, same, prose int }
	counts := map[string]*count{}
	for _, l := range readReproIndex(t) {
		src := strings.SplitN(l.source, " (", 2)[0]
		c := counts[src]
		if c == nil {
			c = &count{}
			counts[src] = c
		}
		switch l.kind {
		case "row":
			c.row++
		case "same":
			c.same++
		case "prose":
			c.prose++
		}
	}
	for _, src := range reproExpectedSources {
		if counts[src] == nil {
			t.Errorf("source %s has no reproduction line", src)
		}
	}
	var names []string
	for s := range counts {
		names = append(names, s)
	}
	sort.Strings(names)
	var sb strings.Builder
	total := count{}
	for _, s := range names {
		c := counts[s]
		fmt.Fprintf(&sb, "\n  %-48s rows %3d  same-as %3d  prose %3d", s, c.row, c.same, c.prose)
		total.row += c.row
		total.same += c.same
		total.prose += c.prose
	}
	t.Logf("reproductions per source file:%s\n  %-48s rows %3d  same-as %3d  prose %3d", sb.String(), "total", total.row, total.same, total.prose)
}
