//go:build dev

package ci

import (
	"strings"
	"testing"
)

// CRW-1540 checks the documented procedure and its authority boundary;
// scenario review separately judges how the instructions apply.
func TestCRW1540TidyConsolidation(t *testing.T) {
	skill := splitRead(t, "plugins/crw/skills/crw-tidy/SKILL.md")
	body := flat(sectionOf(skill, "Consolidate repeated facts"))
	if body == "" {
		t.Fatal("crw-tidy lacks a consolidation procedure")
	}
	last := -1
	for _, step := range []string{
		"1. Find the copies", "2. Choose the canonical home", "3. Propose the changes",
		"4. Replace the copies", "5. Verify the result",
	} {
		at := strings.Index(body, step)
		if at <= last {
			t.Fatalf("missing or out-of-order consolidation step %q", step)
		}
		last = at
	}
	for _, want := range []string{
		"read-only", "ordinary permission to fill clear gaps does not authorize consolidation",
		"Only after approval", "existing approval covers the same scope", "do not ask again",
		"tracking-model.md#where-each-document-type-is-canonical",
		"exact, paraphrased, partial, stale or contradictory", "another search term",
		"unique details", "owner", "contradictions", "stable links",
		"preserve history", "references resolve", "Unverified", "no edits",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consolidation procedure lacks %q", want)
		}
	}
	if !strings.Contains(sectionOf(skill, "Supplement"), "### Consolidate repeated facts") {
		t.Error("consolidation is not under Supplement's ownership and write rules")
	}
}
