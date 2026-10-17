package affordance

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// CRW-1528 changes public wording, not another owner's source, recorded
// artifacts, wire identifiers, or paths that still name real files.
func currentWorkDocument(name string) bool {
	if name == "README.md" {
		return true
	}
	if !strings.HasPrefix(name, "docs/") {
		return false
	}
	for _, prefix := range []string{
		"docs/port-cxc/", "docs/port/", "docs/rebrand/",
		// CRW-1526 owns both the dispatch-case data and its generated skill sections.
		"docs/crw-run/dispatch-cases/", "docs/design/", "docs/flow/script/",
	} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	switch name {
	case "docs/relay/integrator.md", "docs/CI.md", "docs/role-execution-policy.md",
		"docs/runtime-install.md", "docs/skills/inventory-map.json":
		return false
	}
	return true
}

var oldWorkName = regexp.MustCompile(`(?i)\b(?:i?pabcd|crw-pabcd|crw-loop)\b`)

// Retentions are exact text with a maximum count, never an entire code span,
// paragraph or document. Original source paths must still identify their files.
var retainedWorkNames = map[string][]struct {
	text  string
	count int
}{
	"README.md": {
		{"task work (formerly PABCD/IPABCD)", 1},
		{"Continuation (formerly `crw-loop`)", 1},
	},
	"docs/relay/packets.md": {
		{"legacy workflow labels (`crw-loop` and `CXC Loop`)", 1},
	},
	"docs/relay/cxc-contract-map.md": {
		{"`skills/pabcd/references/loop-engineering.md`", 2},
		{"`skills/pabcd/references/delegation.md`", 1},
		{"`skills/pabcd/references/plan-output.md`", 1},
		{"`skills/pabcd/references/phase-control.md`", 1},
		{"`skills/pabcd/references/dispatch-surfaces.md`", 1},
	},
	"docs/relay/risk-tier.md": {
		{"`internal/pabcd/cli/testdata/boundary.json`", 1},
		{"`internal/pabcd/source/session/testdata/scenarios.json`", 1},
		{"`internal/pabcd/gate/testdata`", 1},
	},
}

func oldWorkNameProblems(name, text string) []string {
	for _, keep := range retainedWorkNames[name] {
		text = strings.Replace(text, keep.text, "", keep.count)
	}
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		if match := oldWorkName.FindString(line); match != "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s", name, i+1, match))
		}
	}
	return problems
}

func TestCurrentDocumentsUseTaskWorkNames(t *testing.T) {
	root := filepath.Join("..", "..")
	names := []string{"README.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if name := filepath.ToSlash(rel); currentWorkDocument(name) {
				names = append(names, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			continue // This is a text-document check; binary assets are not prose.
		}
		for _, problem := range oldWorkNameProblems(name, string(data)) {
			t.Error(problem)
		}
	}
}

func TestTaskWorkDocumentCheckKeepsOnlyNamedRetentions(t *testing.T) {
	for _, text := range []string{
		"PABCD work", "IPABCD work", "`crw pabcd orchestrate status`",
		"$crw:crw-pabcd", "`crw-loop`",
	} {
		if problems := oldWorkNameProblems("docs/current.md", text); len(problems) == 0 {
			t.Errorf("old public spelling passed: %s", text)
		}
	}
	for _, name := range []string{
		"README.md", "docs/current.md", "docs/flow/script.md",
		"docs/crw-run/dispatch-cases.md",
	} {
		if !currentWorkDocument(name) {
			t.Errorf("current document excluded: %s", name)
		}
	}
	for _, name := range []string{
		"plugins/crw/skills/crw-run/SKILL.md", "docs/port/a.md", "docs/port-cxc/a.md",
		"docs/rebrand/a.md", "docs/design/a.md", "docs/flow/script/a.md",
		"docs/crw-run/dispatch-cases/recorded/a.md",
		"docs/crw-run/dispatch-cases/contrast/0010-C1.md",
		"docs/crw-run/dispatch-cases/negative/a.md", "docs/relay/integrator.md",
		"docs/CI.md", "docs/role-execution-policy.md", "docs/runtime-install.md",
		"docs/skills/inventory-map.json",
	} {
		if currentWorkDocument(name) {
			t.Errorf("another owner's document included: %s", name)
		}
	}
	const intro = "task work (formerly PABCD/IPABCD)"
	if problems := oldWorkNameProblems("README.md", intro); len(problems) != 0 {
		t.Errorf("first parenthetical name rejected: %v", problems)
	}
	if problems := oldWorkNameProblems("README.md", intro+"\n"+intro); len(problems) == 0 {
		t.Error("repeated old-name explanation passed")
	}
	const source = "`skills/pabcd/references/delegation.md`"
	if problems := oldWorkNameProblems("docs/relay/cxc-contract-map.md", source); len(problems) != 0 {
		t.Errorf("exact upstream source path rejected: %v", problems)
	}
	if problems := oldWorkNameProblems("docs/relay/cxc-contract-map.md", source+"\nUse PABCD."); len(problems) == 0 {
		t.Error("source-path retention hid old prose")
	}
	if problems := oldWorkNameProblems("docs/current.md", source); len(problems) == 0 {
		t.Error("source-path retention leaked to another document")
	}
}
