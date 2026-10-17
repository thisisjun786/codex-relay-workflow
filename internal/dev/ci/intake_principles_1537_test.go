//go:build dev

package ci

import (
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
)

func intake1537Terms(t *testing.T, text string, terms ...string) {
	t.Helper()
	text = strings.ToLower(normalized(text))
	for _, term := range terms {
		if !strings.Contains(text, term) {
			t.Errorf("missing intake principle %q", term)
		}
	}
	if regexp.MustCompile(`\b(readchk|aim|factchk)\b`).MatchString(text) {
		t.Error("source skill names belong in the attribution map, not the instructions")
	}
}

func TestIntakePrinciples1537Define(t *testing.T) {
	text := sectionOf(readRepoText(t, "plugins/crw/skills/crw-define/SKILL.md"), "Find what is unclear")
	intake1537Terms(t, text, "handed-over", "cross-check", "evidence", "before asking", "understood scope", "registration", "inferred intent", "proposal", "stated goal", "remaining", "consequential fork", "one question at a time", "no ritual interview")
}

func TestIntakePrinciples1537Packet(t *testing.T) {
	section := sectionOf(readRepoText(t, "plugins/crw/skills/crw-run/references/task-packet.md"), "Launch packet")
	_, rest, ok := strings.Cut(section, "```text\n")
	if !ok {
		t.Fatal("launch packet has no text template")
	}
	packet, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatal("launch packet template is not closed")
	}
	for _, tc := range []struct {
		field string
		terms []string
	}{
		{"Understood scope", []string{"checked", "handed-over", "decisions", "evidence", "proposal"}},
		{"Remaining fork", []string{"one", "consequential", "none", "owner"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			field := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(tc.field) + `:[^\n]*(?:\n[ \t]+[^\n]*)*`).FindString(packet)
			if field == "" {
				t.Fatalf("template has no %s field", tc.field)
			}
			intake1537Terms(t, field, tc.terms...)
		})
	}
}

func TestIntakePrinciples1537Explorer(t *testing.T) {
	overlay, err := roles.Overlay("explorer", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := sectionOf(overlay, "Intake and evidence")
	intake1537Terms(t, text, "handed-over", "first", "question", "findings", "edit area", "snapshots", "surprising", "apparently obvious", "primary evidence", "path:line", "command output", "missing sources", "unverified", "inferred intent", "proposal", "stated goal", "observed facts", "interpretation", "decisions", "dispatcher", "output schema")
}
