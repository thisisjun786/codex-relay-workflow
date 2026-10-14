//go:build dev

package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1352 (Jun 10-14 04:3x, Sion relaying; design-1316 section 18.1): the session layers are named
// 총괄 (top session), 부모 (parent: the repository session, relay role `supervisor`, the management seat of
// CRW-1314), 자식 (child: the project session, relay role `parent`) and 서브에이전트 (subagents, no role id).
// hierarchy.md holds the one table of those names; the relay role ids, stored fields, flags and CLI words
// stay as they are, so the table warns that the relay's `parent` is the child. The four old layer names
// leave the 1.0-path skills and documents; the records (known-defects, port records, old handoffs) stay as
// written. The dispatch case files under docs/crw-run/dispatch-cases are the source of a skill reference, not records. crw-run-initiative, the initiative lead's skill, leaves the 1.0
// path and the parent's work is folded into crw-run.

// oldLayerName is a layer name the table replaces, in English and in Korean.
var oldLayerName = regexp.MustCompile(`(?i)management session|initiative lead|project orchestrator|project owner|관리 세션|이니셔티브 리드|프로젝트 오케스트레이터|프로젝트 담당`)

// layerNameScanned is whether rel is a 1.0-path skill or document whose text may not use an old layer name:
// the plugin's text files, the documents outside the records, and the top-level documents.
func layerNameScanned(rel string) bool {
	for _, record := range []string{"docs/port/", "docs/port-acr/", "docs/port-cxc/"} {
		if strings.HasPrefix(rel, record) {
			return false
		}
	}
	switch filepath.Ext(rel) {
	case ".md", ".yaml", ".json":
	default:
		return false
	}
	switch {
	case strings.HasPrefix(rel, "plugins/crw/"):
		return true
	case strings.HasPrefix(rel, "docs/"):
		return filepath.Ext(rel) == ".md"
	}
	return rel == "README.md" || rel == "AGENTS.md" || rel == "POLICY.md" || rel == "CONTRIBUTING.md"
}

// layerScanTexts are the scanned files of root by repository-relative path.
func layerScanTexts(t *testing.T, root string) map[string]string {
	t.Helper()
	texts := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case ".git", "node_modules", "web", "internal", "cmd", "testdata", "contract", "port", "packages", "scripts":
				return filepath.SkipDir
			}
			return nil
		}
		if !layerNameScanned(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		texts[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return texts
}

// oldLayerNameProblems are the lines of the texts that still use an old layer name.
func oldLayerNameProblems(texts map[string]string) []string {
	var problems []string
	for rel, text := range texts {
		for i, line := range strings.Split(text, "\n") {
			if m := oldLayerName.FindString(line); m != "" {
				problems = append(problems, rel+":"+strconv.Itoa(i+1)+": "+m)
			}
		}
	}
	return problems
}

func TestOldLayerNamesLeaveTheOnePointZeroPath(t *testing.T) {
	texts := layerScanTexts(t, repoRoot())
	if len(texts) < 100 {
		t.Fatalf("scanned %d files, the plugin and the documents hold more", len(texts))
	}
	for _, p := range oldLayerNameProblems(texts) {
		t.Error(p)
	}
	t.Run("fixture: the records are not scanned and the four names are caught", func(t *testing.T) {
		for _, rel := range []string{"docs/port-cxc/known-defects/CRW-1.md", "docs/port/decisions.md", "docs/port-acr/x.md", "internal/x.go"} {
			if layerNameScanned(rel) {
				t.Errorf("%s is scanned", rel)
			}
		}
		for _, rel := range []string{"plugins/crw/skills/crw-run/SKILL.md", "plugins/crw/skills/crw-run/agents/openai.yaml", "docs/flow/reporting.md", "README.md"} {
			if !layerNameScanned(rel) {
				t.Errorf("%s is not scanned", rel)
			}
		}
		got := oldLayerNameProblems(map[string]string{"a.md": "the Management Session\nan initiative lead\nthe project orchestrator\na project owner\n관리 세션\nthe parent\n"})
		if len(got) != 5 {
			t.Errorf("problems = %v, want five", got)
		}
	})
}

const hierarchyFile = "plugins/crw/skills/crw-canon/references/hierarchy.md"

// TestHierarchyHoldsTheNameTable: the levels section of hierarchy.md is the one table of the names, with the
// columns the issue names, the four rows of design section 18.1 and the warning about the relay's `parent`.
func TestHierarchyHoldsTheNameTable(t *testing.T) {
	text := readRepoText(t, hierarchyFile)
	section := headingSection(text, "## The levels")
	var table [][]string
	for _, candidate := range markdownTables(section) {
		if len(candidate) > 0 && len(candidate[0]) == 5 && strings.EqualFold(candidate[0][0], "Korean") {
			table = candidate
		}
	}
	if table == nil {
		t.Fatal("hierarchy.md's levels section has no table whose first column is Korean")
	}
	header := strings.ToLower(strings.Join(table[0], "|"))
	if header != "korean|english|relay role id|duty|calls" {
		t.Errorf("the table's columns are %q, want Korean, English, Relay role id, Duty, Calls", header)
	}
	rows := map[string][]string{}
	for _, row := range table[1:] {
		rows[row[0]] = row
	}
	for _, want := range []struct{ korean, english, role string }{
		{"총괄", "top session", "none"},
		{"부모", "parent (repository session)", "`supervisor`"},
		{"자식", "child (project session)", "`parent`"},
		{"서브에이전트", "subagents", "none"},
	} {
		row, ok := rows[want.korean]
		if !ok {
			t.Errorf("the table has no row %s", want.korean)
			continue
		}
		if !strings.HasPrefix(strings.ToLower(row[1]), want.english) {
			t.Errorf("row %s: English %q, want it to start with %q", want.korean, row[1], want.english)
		}
		if !strings.HasPrefix(row[2], want.role) {
			t.Errorf("row %s: relay role id %q, want it to start with %s", want.korean, row[2], want.role)
		}
	}
	if len(rows) != 4 {
		t.Errorf("the table has %d rows, want the four layers", len(rows))
	}
	if !strings.Contains(rows["부모"][2], "management seat") || !strings.Contains(rows["부모"][2], "CRW-1314") {
		t.Errorf("the parent's role id does not name the management seat of CRW-1314: %q", rows["부모"][2])
	}
	flatSection := normalized(section)
	for _, want := range []string{
		"The relay role id `parent` names the child, not the parent",
		"The relay role ids `supervisor`, `parent` and `child` are not renamed",
		"architect, executor, reviewer, explorer",
	} {
		if !strings.Contains(flatSection, want) {
			t.Errorf("hierarchy.md's levels section lacks %q", want)
		}
	}
}

// layerLinkProblems: a file that introduces a layer by its English label links the table, and its first use of
// each label carries the relay role id.
func layerLinkProblems(rel, text string) []string {
	if rel == hierarchyFile {
		return nil // the table itself gives each role id in its own column
	}
	flatText := normalized(text)
	var problems []string
	labels := []struct{ label, role string }{
		{"repository session", "relay role `supervisor`"},
		{"project session", "relay role `parent`"},
	}
	uses := false
	for _, l := range labels {
		i := strings.Index(strings.ToLower(flatText), l.label)
		if i < 0 {
			continue
		}
		uses = true
		window := flatText[i+len(l.label) : min(len(flatText), i+len(l.label)+40)]
		if !strings.HasPrefix(strings.TrimLeft(window, ", ("), l.role) {
			problems = append(problems, rel+": the first "+l.label+" does not give "+l.role+": ..."+flatText[i:min(len(flatText), i+80)])
		}
	}
	if uses && !regexp.MustCompile(`\]\([^)]*hierarchy\.md#the-levels\)`).MatchString(text) {
		problems = append(problems, rel+": names a layer and does not link hierarchy.md#the-levels")
	}
	return problems
}

func TestLayerNamesLinkTheTable(t *testing.T) {
	texts := layerScanTexts(t, repoRoot())
	users := 0
	for rel, text := range texts {
		if strings.Contains(strings.ToLower(text), "repository session") || strings.Contains(strings.ToLower(text), "project session") {
			users++
		}
		for _, p := range layerLinkProblems(rel, text) {
			t.Error(p)
		}
	}
	if users < 10 {
		t.Errorf("%d files name a layer by its English label, want the skills that name a layer (at least 10)", users)
	}
	t.Run("fixture", func(t *testing.T) {
		good := "[the child](x/hierarchy.md#the-levels) (project session, relay role `parent`) and the project session again"
		if p := layerLinkProblems("a.md", good); len(p) != 0 {
			t.Errorf("good: %v", p)
		}
		if p := layerLinkProblems("a.md", "the project session, which has a role id elsewhere"); len(p) != 2 {
			t.Errorf("bad: %v", p)
		}
	})
}

// TestRunInitiativeOffThePath: the initiative lead's skill is gone and no 1.0-path file sends anyone to it.
func TestRunInitiativeOffThePath(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot(), "plugins/crw/skills/crw-run-initiative")); !os.IsNotExist(err) {
		t.Errorf("plugins/crw/skills/crw-run-initiative still exists (%v)", err)
	}
	for rel, text := range layerScanTexts(t, repoRoot()) {
		if strings.Contains(text, "crw-run-initiative") {
			t.Errorf("%s still names crw-run-initiative", rel)
		}
	}
}
