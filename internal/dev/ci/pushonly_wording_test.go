//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The push-only procedure (CRW-966): the current rule is a task branch that the one integrator
// fast-forwards to dev, so no document states that a pull request or a hosted gate is required.
// The one place that still describes a pull request lane is the in-flight section of merge-readiness.md,
// and the dated worked example of task-packet.md keeps the old Launch text. The guard reads the skills
// (markdown and agent YAML), the policy documents, the dispatch case data, the generated dispatch document
// and the plugin manifest as text, and fails on a seed phrase anywhere else. A case whose data quotes the
// old procedure on purpose is listed in pushOnlyCaseAllowlist with its reason.

var pushOnlySeedPhrases = regexp.MustCompile("(?i)exactly one pull request|pull request open|pull request is open|open the pull request|open a pull request|open that pull request|open the PR\\b|open a PR\\b|open this PR\\b|open it non-draft|open pull request|opens the pull request|opens a pull request|opens its pull request|opens the PR\\b|intended PR[ ,.]|PR landing|after its CI finishes|on this pull request|dev-gate is required|dev-gate required|dev-gate must|PR body|pull request body|every CI job|pull-request CI|hosted CI run is required|CI run is required|pull request is required|PR is required")

// pushOnlyGradePhrases are the grade-first phrases; a refusal-name table row may keep them, never a pull-request phrase.
var pushOnlyGradePhrases = regexp.MustCompile("(?i)red or security|P0, P1|red, P0|blocking P2")

// pushOnlyCaseAllowlist maps a dispatch case ID to the reason its text may keep the old procedure.
var pushOnlyCaseAllowlist = map[string]string{}

// pushOnlyHistory matches a line that cites a numbered pull request, a dated record of what happened then.
var pushOnlyHistory = regexp.MustCompile("pull request [0-9]+")

const pushOnlyInFlightHeading = "## In-flight pull requests (transition)"
const pushOnlyExampleHeading = "### One issue in both formats"
const pushOnlyGenerated = "plugins/crw/skills/crw-run/references/dispatch-verification.md"
const pushOnlyManifest = "plugins/crw/.codex-plugin/plugin.json"
const pushOnlyInFlightFile = "plugins/crw/skills/crw-run/references/merge-readiness.md"
const pushOnlyExampleFile = "plugins/crw/skills/crw-run/references/task-packet.md"

// pushOnlyExempt names, per file, the one heading whose text may keep a pull-request phrase.
var pushOnlyExempt = map[string]string{
	pushOnlyInFlightFile: pushOnlyInFlightHeading,
	pushOnlyExampleFile:  pushOnlyExampleHeading,
}

var pushOnlyDocs = []string{
	"POLICY.md", "CONTRIBUTING.md", "AGENTS.md", "README.md",
	"docs/CI.md", "docs/releases.md", "docs/plugin-packaging.md", "docs/runtime-install.md",
	"docs/live-trial.md", "docs/role-execution-policy.md",
}

// pushOnlyFiles lists the documents the guard reads, as slash paths relative to the repository root.
func pushOnlyFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot()
	var files []string
	for _, dir := range []string{"plugins/crw/skills", "docs/crw-run/dispatch-cases/recorded", "docs/crw-run/dispatch-cases/negative", "docs/crw-run/dispatch-cases/contrast"} {
		err := filepath.Walk(filepath.Join(root, filepath.FromSlash(dir)), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			agentYAML := strings.HasSuffix(p, ".yaml") && filepath.Base(filepath.Dir(p)) == "agents"
			if !info.IsDir() && (strings.HasSuffix(p, ".md") || agentYAML) {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	files = append(files, pushOnlyGenerated, pushOnlyManifest)
	return append(files, pushOnlyDocs...)
}

// pushOnlyCaseOf returns the dispatch case ID a data file holds, from its <order>-<ID>.md name.
func pushOnlyCaseOf(rel string) string {
	if !strings.Contains(rel, "dispatch-cases/") {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(rel), ".md")
	if i := strings.Index(base, "-"); i >= 0 {
		return base[i+1:]
	}
	return base
}

// pushOnlyLineCase returns the dispatch case ID a line of the generated document opens: a table row
// "| ID | ..." or a case block "**ID — ...". Continuation lines keep the ID of their block.
func pushOnlyLineCase(line string) string {
	if strings.HasPrefix(line, "| ") {
		rest := line[2:]
		if i := strings.Index(rest, " |"); i > 0 && !strings.Contains(rest[:i], " ") {
			return rest[:i]
		}
	}
	if strings.HasPrefix(line, "**") {
		rest := line[2:]
		if i := strings.Index(rest, " — "); i > 0 && !strings.Contains(rest[:i], " ") {
			return rest[:i]
		}
	}
	return ""
}

// pushOnlyOutside reports the lines that carry a seed phrase outside the file's exempt heading and the
// allowlisted cases, as "file:line: text". fileCase is the case ID of a data file; it is empty for a
// document, whose case IDs follow the lines that open them.
func pushOnlyOutside(rel string, text string, fileCase string) []string {
	var hits []string
	h2, h3, lineCase := "", "", ""
	exempt := pushOnlyExempt[rel]
	for i, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			h2, h3, lineCase = line, "", ""
		case strings.HasPrefix(line, "### "):
			h3, lineCase = line, ""
		}
		if id := pushOnlyLineCase(line); id != "" {
			lineCase = id
		}
		if exempt != "" && (h2 == exempt || h3 == exempt) {
			continue
		}
		id := fileCase
		if id == "" {
			id = lineCase
		}
		if _, ok := pushOnlyCaseAllowlist[id]; ok && id != "" {
			continue
		}
		if pushOnlySeedPhrases.MatchString(line) || (pushOnlyGradePhrases.MatchString(line) && !pushOnlyHistory.MatchString(line) && !strings.Contains(line, "the relay grades")) {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
		}
	}
	return hits
}

func TestPushOnlyWording_NoCurrentPullRequestRule(t *testing.T) {
	var hits []string
	for _, rel := range pushOnlyFiles(t) {
		data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, pushOnlyOutside(rel, string(data), pushOnlyCaseOf(rel))...)
	}
	for id, reason := range pushOnlyCaseAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("dispatch case %s is allowlisted without a reason", id)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("current-rule pull-request wording outside the in-flight section:\n%s", strings.Join(hits, "\n"))
	}
}

// TestPushOnlyWording_GuardScope pins the guard itself: each phrase form is caught, the exemption applies
// only to its own file and heading, and the agent YAML and the plugin manifest are scanned.
func TestPushOnlyWording_GuardScope(t *testing.T) {
	const skill = "plugins/crw/skills/crw-run/SKILL.md"
	cases := []struct {
		name string
		rel  string
		text string
		want int
	}{
		{"open the PR in a skill", skill, "Then open the PR for review.", 1},
		{"open a pull request in a skill", skill, "Then open a pull request.", 1},
		{"hosted CI run is required", skill, "A hosted CI run is required before the handoff.", 1},
		{"agent yaml prompt", "plugins/crw/skills/crw-run/agents/openai.yaml", "default_prompt: open the PR", 1},
		{"plugin description", pushOnlyManifest, "  \"description\": \"open the PR\"", 1},
		{"in-flight section is exempt", pushOnlyInFlightFile, pushOnlyInFlightHeading + "\n\nopen a pull request\n", 0},
		{"same phrase under another heading of the in-flight file", pushOnlyInFlightFile, "## Other\n\nopen a pull request\n", 1},
		{"in-flight exemption does not reach another file", skill, pushOnlyInFlightHeading + "\n\nopen a pull request\n", 1},
		{"worked example is exempt", pushOnlyExampleFile, pushOnlyExampleHeading + "\n\nopen a pull request\n", 0},
		{"same phrase under another heading of the example file", pushOnlyExampleFile, "## Other\n\nopen a pull request\n", 1},
	}
	for _, c := range cases {
		if got := pushOnlyOutside(c.rel, c.text, ""); len(got) != c.want {
			t.Errorf("%s: got %d hits %q, want %d", c.name, len(got), got, c.want)
		}
	}
}

func TestPushOnlyWording_ScansAgentYAMLAndManifest(t *testing.T) {
	files := pushOnlyFiles(t)
	for _, want := range []string{pushOnlyManifest, "plugins/crw/skills/crw-run/agents/openai.yaml"} {
		found := false
		for _, f := range files {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("guard does not scan %s", want)
		}
	}
}

func TestPushOnlyWording_InFlightSectionIsSingle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(pushOnlyInFlightFile)))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"+pushOnlyInFlightHeading+"\n"); n != 1 {
		t.Fatalf("merge-readiness.md has %d in-flight sections, want exactly one", n)
	}
}
