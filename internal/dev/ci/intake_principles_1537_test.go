//go:build dev

package ci

import (
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
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
	var role struct {
		Instructions string `toml:"developer_instructions"`
	}
	if _, err := toml.Decode(readRepoText(t, "internal/role/agents/explorer.toml"), &role); err != nil {
		t.Fatal(err)
	}
	method := sectionOf(role.Instructions, "Method")
	intake1537Terms(t, method, "handed-over", "first", "question", "findings", "edit area", "snapshots", "surprising", "apparently obvious", "primary evidence", "path:line", "command output", "missing sources", "unverified", "inferred intent", "proposal", "stated goal")
	output := sectionOf(role.Instructions, "Output")
	intake1537Terms(t, output, "primary evidence", "observed facts", "interpretation", "unverified", "decisions", "dispatcher")
}
