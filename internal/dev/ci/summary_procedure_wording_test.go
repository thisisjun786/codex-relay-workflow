//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1344: the coordination summary of a DAG plan is one GitHub comment, and GitHub has no compare-and-swap on a comment
// edit. The procedure in crw-run/references/relay.md therefore (a) re-reads the comment and takes a live claim right before
// every write, (b) says plainly that this narrows the race window and does not close it, and (c) repairs a comment that
// holds a stale summary with the relay's existing commands (dag-summary-status, dag-summary-reconcile, dag-summary-enqueue
// --again) instead of stopping. It also no longer calls the write a conditional replacement.

func summarySection(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), skillsDir, "crw-run/references/relay.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	const head = "## The coordination summary of a DAG plan"
	i := strings.Index(text, head)
	if i < 0 {
		t.Fatal("relay.md has no DAG summary procedure")
	}
	rest := text[i+len(head):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestSummaryProcedureWritesAgainstAFreshReadAndRepairsAStaleComment(t *testing.T) {
	section := summarySection(t)
	for _, want := range []string{
		"read the comment again", // immediately before every PATCH
		"dag-summary-status",     // the live-claim check right before the PATCH and the convergence repair
		"narrow the window between the read and the write and do not close it", // GitHub has no compare-and-swap
		"two writers inside the window can still overwrite each other",
		"converge to the newest confirmed summary",
		"dag-summary-reconcile",
		"enqueue --plan <plan> --actor <you> --document <doc> --again",
		"dag-summary-fail",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the DAG summary procedure does not say %q", want)
		}
	}
	// step 3 (initialize) describes the GitHub comment write, not a Linear patch
	for _, banned := range []string{"conditional", "atomic", "patch of", "Linear"} {
		if strings.Contains(section, banned) {
			t.Errorf("the DAG summary procedure still says %q", banned)
		}
	}
	// the stale-summary dead end: a sync_not_claimable answer must not end at "Nothing owed and up to date: stop" with the comment unchecked
	if !strings.Contains(section, "sync_not_claimable") || !strings.Contains(section, "stale summary") {
		t.Error("the procedure does not route sync_not_claimable to a check of the comment against the newest confirmed summary")
	}
}

func TestSummaryWriteIsNamedWithoutConditional(t *testing.T) {
	for _, rel := range []string{"crw-run/references/relay.md", "crw-canon/references/delivery.md"} {
		data, err := os.ReadFile(filepath.Join(repoRoot(), skillsDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			low := strings.ToLower(line)
			if strings.Contains(low, "conditional") && (strings.Contains(low, "container") || strings.Contains(low, "replacement")) {
				t.Errorf("%s:%d still calls the summary write conditional: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
	}
}
