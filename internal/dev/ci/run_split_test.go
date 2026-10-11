//go:build dev

package ci

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The project orchestrator procedure stays in crw-run; the initiative lead's part (initiative supervision and the midpoint
// check) is crw-run-initiative, which links the references that stay under crw-run. crw-next is part of crw-status and
// crw-logic part of crw-check. These checks pin that layout (CRW-1218).

const (
	runSkillDir        = "plugins/crw/skills/crw-run"
	runInitiativeDir   = "plugins/crw/skills/crw-run-initiative"
	runSkillFile       = runSkillDir + "/SKILL.md"
	runInitiativeSkill = runInitiativeDir + "/SKILL.md"
	relaySpecsPath     = "internal/relay/argparse/specs.json"
)

func splitRead(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func splitExists(rel string) bool {
	_, err := os.Stat(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	return err == nil
}

func splitLinesContaining(text, needle string) []int {
	var found []int
	for i, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			found = append(found, i+1)
		}
	}
	return found
}

// TestRunInitiativePointerSingleLine: crw-run keeps one line that sends an initiative target to
// crw-run-initiative, and the initiative procedure itself is not in crw-run.
func TestRunInitiativePointerSingleLine(t *testing.T) {
	run := splitRead(t, runSkillFile)
	// The frontmatter description may name the skill it routes to; the body holds one pointer line.
	body := run[strings.Index(run[4:], "\n---\n")+8:]
	if lines := splitLinesContaining(body, "crw-run-initiative"); len(lines) != 1 {
		t.Errorf("the body of %s names crw-run-initiative on lines %v of the body, want exactly one pointer line", runSkillFile, lines)
	} else if line := strings.Split(body, "\n")[lines[0]-1]; !strings.Contains(line, "](../crw-run-initiative/SKILL.md)") {
		t.Errorf("%s:%d does not link ../crw-run-initiative/SKILL.md: %s", runSkillFile, lines[0], line)
	}
	for _, heading := range headingsOf(run) {
		if strings.Contains(strings.ToLower(heading), "initiative") || strings.Contains(strings.ToLower(heading), "midpoint") {
			t.Errorf("%s keeps the heading %q: the initiative procedure and the midpoint check are crw-run-initiative's", runSkillFile, heading)
		}
	}
	for _, gone := range []string{"Initiative supervision", "initiative-supervision.md", "**Supervise an initiative:**", "**Status only:**", "midpoint-check.md"} {
		if strings.Contains(run, gone) {
			t.Errorf("%s still contains %q", runSkillFile, gone)
		}
	}
	if splitExists("plugins/crw/skills/crw-run-project") {
		t.Error("plugins/crw/skills/crw-run-project exists: crw-run is the project orchestrator skill")
	}
	initiative := splitRead(t, runInitiativeSkill)
	for _, want := range []string{"](../crw-run/references/initiative-supervision.md)", "](../crw-run/references/midpoint-check.md)", "](../crw-run/SKILL.md)"} {
		if !strings.Contains(initiative, want) {
			t.Errorf("%s does not link %s", runInitiativeSkill, want)
		}
	}
	if !splitExists(runInitiativeDir + "/agents/openai.yaml") {
		t.Errorf("%s has no agents/openai.yaml", runInitiativeDir)
	}
}

// runCommandSequenceBefore is the order in which crw-run/SKILL.md names a relay command (a name of
// internal/relay/argparse/specs.json written in code font) at a52e7ff09, before the split. The
// initiative procedure named none of them, so the sequence is the project procedure's.
var runCommandSequenceBefore = []string{
	"doctor", "dispositions-show", "managed-start", "managed-start", "dag-release", "managed-start", "managed-start",
	"dag-bundle-candidates", "managed-start", "dag-ready", "dag-integration-observe", "dag-accept", "dag-conflict-sweep",
	"dag-ready", "merge-evidence", "decision-reply", "dag-correct", "admit-turn", "linkage-handover", "linkage-completion",
	"relationship-close-merged", "supervisor-standing",
}

// relayCommandSequence is the relay commands text names, in order of appearance.
func relayCommandSequence(t *testing.T, text string) []string {
	t.Helper()
	var specs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(splitRead(t, relaySpecsPath)), &specs); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, spec := range specs {
		if spec.Name != "" {
			names = append(names, regexp.QuoteMeta(spec.Name))
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	pattern := regexp.MustCompile("`(?:crw )?(" + strings.Join(names, "|") + ")[ `]")
	var out []string
	for _, m := range pattern.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestRunCommandSequenceUnchanged: crw-run names the same relay commands in the same order as before the split.
func TestRunCommandSequenceUnchanged(t *testing.T) {
	expectEqual(t, "relay commands of "+runSkillFile, relayCommandSequence(t, splitRead(t, runSkillFile)), runCommandSequenceBefore)
	// The initiative procedure sends a project to its orchestrator; it runs none of the project's release or
	// close commands itself.
	for _, command := range relayCommandSequence(t, splitRead(t, runInitiativeSkill)) {
		switch command {
		case "managed-start", "dag-release", "dag-correct", "dag-accept", "relationship-close-merged":
			t.Errorf("%s runs the project command %s", runInitiativeSkill, command)
		}
	}
}

// TestAbsorbedSkillsMoved: crw-next and crw-logic have no directory, and nothing in the repository
// (links, tests, the inventory map, the skillport records, the manifests) names them.
func TestAbsorbedSkillsMoved(t *testing.T) {
	root := repoRoot()
	for _, name := range []string{"crw-next", "crw-logic"} {
		if splitExists("plugins/crw/skills/" + name) {
			t.Errorf("plugins/crw/skills/%s still exists", name)
		}
		if splitExists("port/cxc/records/" + name + ".json") {
			t.Errorf("port/cxc/records/%s.json still exists", name)
		}
	}
	// The end condition the CRW-1218 comment adds: the merge leaves 34 skill directories or fewer.
	entries, err := os.ReadDir(filepath.Join(root, "plugins/crw/skills"))
	if err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, entry := range entries {
		if entry.IsDir() {
			dirs++
		}
	}
	if dirs > 34 {
		t.Errorf("plugins/crw/skills has %d skill directories, want at most 34", dirs)
	}
	files, err := repositoryFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`\bcrw-(?:next|logic)\b`)
	self := "internal/dev/ci/run_split_test.go"
	for _, name := range files {
		switch {
		case name == self, strings.HasPrefix(name, "testdata/"), strings.HasPrefix(name, "docs/port-cxc/known-defects/"):
			continue
		case !(strings.HasPrefix(name, "plugins/") || strings.HasPrefix(name, "docs/") || strings.HasPrefix(name, "port/") ||
			strings.HasPrefix(name, "internal/") || strings.HasPrefix(name, "cmd/") || name == "README.md"):
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if named.MatchString(line) {
				t.Errorf("%s:%d names an absorbed skill: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestMidpointTriggerSingleOwner: the midpoint check has one owner, crw-run-initiative: its trigger word is in
// that description alone, its procedure is in that skill and one reference, and crw-status holds neither.
func TestMidpointTriggerSingleOwner(t *testing.T) {
	descriptions, err := skillDescriptions(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	for name, description := range descriptions {
		has := strings.Contains(description, "중간점검")
		if has != (name == "crw-run-initiative") {
			t.Errorf("%s: the trigger 중간점검 in the description is %v, want it in crw-run-initiative alone", name, has)
		}
	}
	const heading = "Long-run midpoint check"
	owners := 0
	err = filepath.WalkDir(filepath.Join(repoRoot(), "plugins/crw/skills"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "SKILL.md" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, h := range headingsOf(string(data)) {
			if h == heading {
				owners++
				if filepath.Base(filepath.Dir(path)) != "crw-run-initiative" {
					t.Errorf("%s holds the heading %q", path, heading)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Errorf("%d SKILL.md files hold the heading %q, want one (crw-run-initiative)", owners, heading)
	}
	if splitExists("plugins/crw/skills/crw-status/references/midpoint-check.md") {
		t.Error("crw-status still has references/midpoint-check.md")
	}
	if !splitExists(runSkillDir + "/references/midpoint-check.md") {
		t.Error("the midpoint reference is not under crw-run/references")
	}
	status := splitRead(t, "plugins/crw/skills/crw-status/SKILL.md")
	for _, heading := range headingsOf(status) {
		if strings.Contains(strings.ToLower(heading), "checkpoint") {
			t.Errorf("crw-status keeps the heading %q: status is read-only and has no checkpoint branch", heading)
		}
	}
	if strings.Contains(status, "midpoint-check.md") {
		t.Error("crw-status links the midpoint reference")
	}
}

// pinnedRunPath is a path below crw-run that Go code or a Go test pins.
var pinnedRunPath = regexp.MustCompile(`crw-run/((?:references|scripts)/[A-Za-z0-9_./*-]*[A-Za-z0-9*]|SKILL\.md)`)

// TestRunSplitPinnedPathsResolve: the crw-run paths the Go code pins still resolve (no path moved), the links crw-run
// must keep resolve, and crw-run-initiative links its references below crw-run.
func TestRunSplitPinnedPathsResolve(t *testing.T) {
	root := repoRoot()
	pinned := []string{
		"plugins/skill_assets.go", "internal/skill/common.go", "internal/dev/ci/contracts.go", "internal/dev/ci/dispatch_cases.go",
		"internal/dev/ci/plugin_test.go", "internal/relay/dagsched/outbox_docs_test.go", "internal/relay/dagsched/sweep_docs_test.go",
		"internal/relay/dagsched/policy_docs_test.go", "internal/relay/delivery/marker.go",
	}
	for _, file := range pinned {
		matches := pinnedRunPath.FindAllStringSubmatch(splitRead(t, file), -1)
		if file == "internal/dev/ci/plugin_test.go" || file == "internal/dev/ci/contracts.go" {
			// These pin the skill directory by a prefix, or build fixture files below it that exist only in a
			// temporary checkout; the skill itself must still be the one they name.
			if !strings.Contains(splitRead(t, file), "crw-run") {
				t.Errorf("%s no longer names crw-run", file)
			}
			continue
		}
		if len(matches) == 0 {
			t.Errorf("%s names no crw-run path: the pin moved", file)
		}
		for _, m := range matches {
			if m[1] == "SKILL.md" || !strings.Contains(m[1], "*") {
				if !splitExists(runSkillDir + "/" + m[1]) {
					t.Errorf("%s pins crw-run/%s, which does not exist", file, m[1])
				}
				continue
			}
			if found, _ := filepath.Glob(filepath.Join(root, runSkillDir, filepath.FromSlash(m[1]))); len(found) == 0 {
				t.Errorf("%s pins crw-run/%s, which matches no file", file, m[1])
			}
		}
	}
	run := splitRead(t, runSkillFile)
	cache := map[string]map[string]bool{}
	for _, link := range []string{"references/relay.md#the-linear-summary-of-a-dag-plan", "references/region-grades.md#read-the-merge-order"} {
		if !strings.Contains(run, "]("+link+")") {
			t.Errorf("%s does not keep the link %s", runSkillFile, link)
			continue
		}
		file, fragment, _ := strings.Cut(link, "#")
		anchors, err := anchorsOf(cache, filepath.Join(root, runSkillDir, filepath.FromSlash(file)))
		if err != nil || !anchors[fragment] {
			t.Errorf("%s: the anchor #%s does not resolve in %s (%v)", runSkillFile, fragment, file, err)
		}
	}
	links := regexp.MustCompile(`\]\((\.\./crw-run/[^)#]+)(#[^)]*)?\)`).FindAllStringSubmatch(splitRead(t, runInitiativeSkill), -1)
	if len(links) == 0 {
		t.Errorf("%s links nothing below ../crw-run/", runInitiativeSkill)
	}
	for _, m := range links {
		target := filepath.Join(root, runInitiativeDir, filepath.FromSlash(m[1]))
		if _, err := os.Stat(target); err != nil {
			t.Errorf("%s links %s, which does not exist", runInitiativeSkill, m[1])
			continue
		}
		if m[2] != "" {
			if anchors, err := anchorsOf(cache, target); err != nil || !anchors[strings.TrimPrefix(m[2], "#")] {
				t.Errorf("%s: the anchor %s does not resolve in %s", runInitiativeSkill, m[2], m[1])
			}
		}
	}
	// references/ and scripts/ stay below crw-run.
	for _, dir := range []string{"references", "scripts"} {
		if splitExists(runInitiativeDir + "/" + dir) {
			t.Errorf("%s/%s exists: references and scripts stay below crw-run", runInitiativeDir, dir)
		}
		if !splitExists(runSkillDir + "/" + dir) {
			t.Errorf("%s/%s is gone", runSkillDir, dir)
		}
	}
}
