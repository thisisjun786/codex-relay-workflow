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
// The one place that still describes a pull request is the in-flight section of merge-readiness.md.
// The guard reads the skill and policy documents as text and fails on a seed phrase outside
// that section, the worked example of the Launch packet (a dated history), and the generated
// dispatch-verification document (its case data is the source and quotes old text on purpose).

var pushOnlySeedPhrases = regexp.MustCompile(`(?i)exactly one pull request|pull request open|pull request is open|open the pull request|open a pull request|open that pull request|open it non-draft|\bintended PR\b|\bPR landing\b|after its CI finishes|on this pull request|dev-gate is required|PR body|pull request body|every CI job|pull-request CI`)

const pushOnlyInFlightHeading = "## In-flight pull requests (transition)"
const pushOnlyExampleHeading = "### One issue in both formats"
const pushOnlyGenerated = "plugins/crw/skills/crw-run/references/dispatch-verification.md"

var pushOnlyDocs = []string{
	"POLICY.md", "CONTRIBUTING.md", "AGENTS.md", "README.md",
	"docs/CI.md", "docs/releases.md", "docs/plugin-packaging.md", "docs/runtime-install.md",
	"docs/live-trial.md", "docs/role-execution-policy.md",
}

// pushOnlyFiles lists the documents the guard reads: every markdown file under the skills
// directory and the policy and user documents above.
func pushOnlyFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot()
	var files []string
	err := filepath.Walk(filepath.Join(root, "plugins", "crw", "skills"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".md") {
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
	return append(files, pushOnlyDocs...)
}

// pushOnlyOutside reports the lines of text that carry a seed phrase outside the in-flight
// section and the worked example, as "line: text" strings.
func pushOnlyOutside(rel string, text string) []string {
	var hits []string
	section := ""
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			section = line
		} else if strings.HasPrefix(line, "### ") && section != pushOnlyInFlightHeading {
			if strings.HasPrefix(line, pushOnlyExampleHeading) {
				section = "example"
			} else if section == "example" {
				section = ""
			}
		}
		if section == pushOnlyInFlightHeading || section == "example" {
			continue
		}
		if pushOnlySeedPhrases.MatchString(line) {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
		}
	}
	return hits
}

func TestPushOnlyWording_NoCurrentPullRequestRule(t *testing.T) {
	var hits []string
	for _, rel := range pushOnlyFiles(t) {
		if rel == pushOnlyGenerated {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, pushOnlyOutside(rel, string(data))...)
	}
	if len(hits) > 0 {
		t.Fatalf("current-rule pull-request wording outside the in-flight section:\n%s", strings.Join(hits, "\n"))
	}
}

func TestPushOnlyWording_InFlightSectionIsSingle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "plugins", "crw", "skills", "crw-run", "references", "merge-readiness.md"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"+pushOnlyInFlightHeading+"\n"); n != 1 {
		t.Fatalf("merge-readiness.md has %d in-flight sections, want exactly one", n)
	}
}
