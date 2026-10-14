//go:build dev

package ci

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// CRW-1323: the gaps a Codex session hit while following the skills in the 10-13 trial (CRW-1315 items 2,
// 3, 12, 21, 27 and 31). The parent (repository session) finds its work and its commands in one skill; the
// project creation path says what a flow run needs before it starts; a request example carries the fields
// the host accepts; a skill's relative links open in the installed plugin, which ships the plugin
// directory and no docs/**; and a skill says how to leave temporary files without rm.

const runSkillPath = "plugins/crw/skills/crw-run/SKILL.md"

// TestParentWorkInOneSkill: crw-run holds the parent's section: what it decides (project order, which children
// run together, the answer to a decision, the reports it reads) and the commands it runs.
func TestParentWorkInOneSkill(t *testing.T) {
	text := readRepoText(t, runSkillPath)
	if !strings.Contains(text, "\n## The parent: the repository session\n") {
		t.Fatal("crw-run has no section \"The parent: the repository session\"")
	}
	section := normalized(headingSection(text, "## The parent: the repository session"))
	for _, want := range []string{
		"[the parent](../crw-canon/references/hierarchy.md#the-levels) (repository session, relay role `supervisor`)",
		"project order", "which children run together", "answers a decision", "reports",
		"`crw manage supervisor register`", "`crw flow reports --unread`", "`crw flow report-ack REPORT --turn TURN`",
		"`crw manage checkpoint`", "never operates a run",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the parent's section lacks %q", want)
		}
	}
	for _, heading := range []string{"### Long-run midpoint check", "### Answer a 중간점검 request", "### Read the milestone reports"} {
		if strings.Count(text, "\n"+heading+"\n") != 1 {
			t.Errorf("crw-run does not hold the heading %q once", heading)
		}
	}
}

// TestProjectCreationStatesFlowPrerequisites: crw-plan, which creates the project, says what a flow run of it needs.
func TestProjectCreationStatesFlowPrerequisites(t *testing.T) {
	section := normalized(headingSection(readRepoText(t, "plugins/crw/skills/crw-plan/SKILL.md"), "## Before a flow run starts"))
	for _, want := range []string{"`milestones`", "`crw flow compile`", "`dag-plan-put`", "`crw flow start`", "refused"} {
		if !strings.Contains(section, want) {
			t.Errorf("crw-plan's flow prerequisites lack %q", want)
		}
	}
}

// managedStartExamples are the JSON blocks of text that hold a managed-start/1 request.
func managedStartExamples(text string) []string {
	var out []string
	for _, block := range regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(text, -1) {
		if strings.Contains(block[1], `"managed-start/1"`) {
			out = append(out, block[1])
		}
	}
	return out
}

// workspacePaths points the example's placeholder workspace paths (/workspace/...) at directories that exist,
// since the relay parses a request against the host's paths.
func workspacePaths(t *testing.T, example string) string {
	t.Helper()
	root := t.TempDir()
	return regexp.MustCompile(`"/workspace/[^"]*"`).ReplaceAllStringFunc(example, func(quoted string) string {
		dir := filepath.Join(root, strings.Trim(quoted, `"`))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return `"` + dir + `"`
	})
}

// TestManagedStartExamplesCarryMcpProfile: every managed-start request example parses as the relay parses a request
// and states the child's MCP profile, and the skill holds one of them.
func TestManagedStartExamplesCarryMcpProfile(t *testing.T) {
	root := repoRoot()
	inSkill := 0
	for _, dir := range []string{"plugins/crw", "docs"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if strings.HasPrefix(filepath.ToSlash(rel), "docs/port") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, example := range managedStartExamples(string(data)) {
				if strings.HasPrefix(filepath.ToSlash(rel), "plugins/") {
					inSkill++
				}
				request, err := managed.ParseRequest([]byte(workspacePaths(t, example)))
				if err != nil {
					t.Errorf("%s: the managed-start example does not parse: %v", rel, err)
					continue
				}
				var shape struct {
					Child struct {
						Settings struct {
							MCPProfile string `json:"mcpProfile"`
						} `json:"settings"`
					} `json:"child"`
				}
				raw, _ := json.Marshal(request)
				if json.Unmarshal(raw, &shape) != nil || shape.Child.Settings.MCPProfile == "" {
					t.Errorf("%s: the managed-start example states no child.settings.mcpProfile", rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if inSkill == 0 {
		t.Error("no skill holds a managed-start request example")
	}
}

// payloadLinkProblems are the relative links of the plugin's Markdown files whose target is outside the plugin
// directory or missing from it.
func payloadLinkProblems(pluginRoot string) []string {
	var problems []string
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	_ = filepath.WalkDir(pluginRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(pluginRoot, path)
		for _, m := range link.FindAllStringSubmatch(string(data), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if target == "" {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
			if inside, _ := filepath.Rel(pluginRoot, resolved); strings.HasPrefix(inside, "..") {
				problems = append(problems, filepath.ToSlash(rel)+": "+m[1]+" is outside the plugin payload")
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				problems = append(problems, filepath.ToSlash(rel)+": "+m[1]+" does not open")
			}
		}
		return nil
	})
	return problems
}

func TestSkillLinksResolveInThePluginPayload(t *testing.T) {
	for _, p := range payloadLinkProblems(filepath.Join(repoRoot(), "plugins/crw")) {
		t.Error(p)
	}
	t.Run("fixture: a link out of the payload and a missing target are reported", func(t *testing.T) {
		root := t.TempDir()
		plugin := filepath.Join(root, "plugin")
		for _, f := range []string{"plugin/skills/a/SKILL.md", "plugin/skills/a/references/r.md", "docs/d.md"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		body := "[r](references/r.md#x) [d](../../../docs/d.md) [gone](references/gone.md) [web](https://x) [here](#y)"
		if err := os.WriteFile(filepath.Join(plugin, "skills/a/SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"plugin/skills/a/references/r.md", "docs/d.md"} {
			if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if p := payloadLinkProblems(plugin); len(p) != 2 {
			t.Errorf("problems = %v, want the docs link and the missing file", p)
		}
	})
}

// TestTemporaryFilesWithoutRm: crw-run says in one line how a session leaves its temporary files without rm, which
// the Codex exec policy refuses.
func TestTemporaryFilesWithoutRm(t *testing.T) {
	var lines []string
	for _, line := range strings.Split(readRepoText(t, runSkillPath), "\n") {
		if strings.Contains(line, "`rm -f`") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("crw-run names `rm -f` on %d lines, want the one cleanup line", len(lines))
	}
	for _, want := range []string{"`mktemp -d`", "exec policy", "`*** Delete File:`"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the cleanup line lacks %q: %s", want, lines[0])
		}
	}
}
