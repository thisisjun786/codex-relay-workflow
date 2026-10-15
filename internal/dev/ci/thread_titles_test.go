//go:build dev

package ci

import (
	"strings"
	"testing"
)

// CRW-1313: hierarchy.md holds the one table of thread titles, a row per layer: the top session (CRW creates it
// not, so no rule), the parent (the repository session, `[CRW] 부모 · <owner>/<repo>`), the child (the project
// session, crw-plan's app presentation rule) and the subagents
// (no title). It says who names a thread and when (the creator, right after creation, with thread/name/set, never the
// first prompt sentence) and that a naming failure warns and records. The documents that keep a layer's rule link the
// table, and no other document spells the repository session's title.
func TestThreadTitlesAreOneTable(t *testing.T) {
	section := headingSection(readRepoText(t, hierarchyFile), "## Thread titles")
	if section == "" {
		t.Fatal("hierarchy.md has no section \"Thread titles\"")
	}
	var table [][]string
	for _, candidate := range markdownTables(section) {
		if len(candidate) > 0 && strings.EqualFold(candidate[0][0], "Layer") {
			table = candidate
		}
	}
	if len(table) != 5 {
		t.Fatalf("the thread titles table has %d lines, want a header and four layers: %v", len(table), table)
	}
	rows := map[string]string{}
	for _, row := range table[1:] {
		rows[strings.Fields(row[0])[0]] = strings.Join(row[1:], " | ")
	}
	for layer, want := range map[string][]string{
		"총괄":     {"CRW does not create it"},
		"부모":     {"`[CRW] 부모 · <owner>/<repo>`", "`create_thread`"},
		"자식":     {"set-the-app-presentation-and-record", "`create_thread`"},
		"서브에이전트": {"no title"},
	} {
		row, ok := rows[layer]
		if !ok {
			t.Errorf("the thread titles table has no row %s: %v", layer, rows)
			continue
		}
		for _, w := range want {
			if !strings.Contains(row, w) {
				t.Errorf("row %s lacks %q: %s", layer, w, row)
			}
		}
	}
	flat := normalized(section)
	for _, want := range []string{
		"names it right after `thread/start` with App Server `thread/name/set`",
		"never the first prompt sentence",
		"warns and records",
		"never refused",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the thread titles section lacks %q", want)
		}
	}
	for _, rel := range []string{"plugins/crw/skills/crw-plan/references/integrations.md", "plugins/crw/skills/crw-run/references/task-packet.md", "plugins/crw/skills/crw-run/references/bridge.md"} {
		if !strings.Contains(readRepoText(t, rel), "hierarchy.md#thread-titles)") {
			t.Errorf("%s does not link the thread titles table", rel)
		}
	}
	for rel, text := range layerScanTexts(t, repoRoot()) {
		if rel != hierarchyFile && strings.Contains(text, "[CRW] 부모") {
			t.Errorf("%s spells the repository session's title outside the table", rel)
		}
	}
}
