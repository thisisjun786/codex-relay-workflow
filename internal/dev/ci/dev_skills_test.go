//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The development, search and tool skills (CRW-1220): crw-dev is a classifier and router, each of the
// neighbouring skills has one role, the repository governance lives in crw-canon, the Luna lane is a
// reference of crw-search and the worktree kinds have one owner each. The checks read the structure the
// routing rests on (descriptions, table rows, headings, links), not the sentences around it.

const devSkillsRoot = "plugins/crw/skills/"

// The paths each check test reads; rowTestReads registers them, and the tests below read them from these lists.
var (
	crwDevRoutingReads   = []string{"plugins/crw/skills/crw-dev/SKILL.md", "plugins/crw/skills/crw-qa/SKILL.md", "plugins/crw/skills/crw-search/SKILL.md", "plugins/crw/skills/crw-recall/SKILL.md"}
	reviewerWordingReads = []string{"plugins/crw/skills/crw-dev-code-reviewer/", "plugins/crw/skills/crw-dev-debugging/", "plugins/crw/skills/crw-dev/SKILL.md"}
	worktreeOwnerReads   = []string{"plugins/crw/skills/crw-worktree-guardian/SKILL.md", "plugins/crw/skills/crw-dev-devops/SKILL.md", "plugins/crw/skills/crw-run/SKILL.md"}
	governanceReads      = []string{"plugins/crw/skills/crw-dev-devops/", "plugins/crw/skills/crw-canon/references/", "plugins/crw/skills/crw-dev/references/"}
)

// skillText is the file at the repository-relative path, or the error as a test failure.
func skillText(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var (
	crwSkillName = regexp.MustCompile("`(crw-[a-z0-9-]+)`")
	mustUseWords = regexp.MustCompile(`(?i)\bmust use\b`)
)

// catchAllErrors is the catch-all wording a description of the four formerly implicit-on skills (crw-dev,
// crw-qa, crw-search, crw-recall) does not carry: a MUST USE claim, and a length over the limit.
func catchAllErrors(name, description string) []string {
	var errs []string
	if mustUseWords.MatchString(description) {
		errs = append(errs, name+": the description claims MUST USE")
	}
	if n := utf8.RuneCountInString(description); n > maxDescriptionRunes {
		errs = append(errs, name+": the description has "+strconv.Itoa(n)+" characters, over "+strconv.Itoa(maxDescriptionRunes))
	}
	return errs
}

// crwDevDescriptionErrors is what the crw-dev description is: a classifier and router (it names C0-C5 and the
// crw-dev-* owners it loads), with none of the neighbours' triggers.
func crwDevDescriptionErrors(description string) []string {
	errs := catchAllErrors("crw-dev", description)
	for _, want := range []string{"C0-C5", "crw-dev-*"} {
		if !strings.Contains(description, want) {
			errs = append(errs, "crw-dev: the description does not name "+want)
		}
	}
	for _, word := range []string{"review", "merge", "QA", "refactor", "scaffolding", "PR creation", "stacked"} {
		if strings.Contains(strings.ToLower(description), strings.ToLower(word)) {
			errs = append(errs, "crw-dev: the description carries the neighbour trigger "+word)
		}
	}
	return errs
}

// executorRowErrors reads the router's executor row: the skills it loads are crw-dev and crw-dev-* skills that
// exist, and never the loop or PABCD skills.
func executorRowErrors(root, text string) []string {
	var found [][]string
	for _, row := range tableRows(text, "Caller") {
		if len(row) >= 2 && strings.HasPrefix(row[0], "executor") {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		return []string{"crw-dev: the router has " + strconv.Itoa(len(found)) + " executor rows, want 1"}
	}
	var errs []string
	loads := crwSkillName.FindAllStringSubmatch(found[0][1], -1)
	if len(loads) == 0 {
		errs = append(errs, "crw-dev: the executor row loads no skill by name")
	}
	hasDev := false
	for _, m := range loads {
		name := m[1]
		hasDev = hasDev || name == "crw-dev"
		if name != "crw-dev" && !strings.HasPrefix(name, "crw-dev-") {
			errs = append(errs, "crw-dev: the executor row loads "+name+", which is not a crw-dev-* skill")
			continue
		}
		if _, err := os.Stat(filepath.Join(root, devSkillsRoot, name, "SKILL.md")); err != nil {
			errs = append(errs, "crw-dev: the executor row names "+name+", which has no SKILL.md")
		}
	}
	if !hasDev {
		errs = append(errs, "crw-dev: the executor row does not load crw-dev")
	}
	return errs
}

func TestCrwDevDescriptionNoCatchAll(t *testing.T) {
	t.Run("the checked-in skills", func(t *testing.T) {
		root := repoRoot()
		var errs []string
		for _, rel := range crwDevRoutingReads {
			fields, err := frontmatterFields(skillText(t, root, rel))
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(filepath.Dir(rel))
			if name == "crw-dev" {
				errs = append(errs, crwDevDescriptionErrors(fields["description"])...)
			} else {
				errs = append(errs, catchAllErrors(name, fields["description"])...)
			}
		}
		errs = append(errs, executorRowErrors(root, skillText(t, root, devSkillsRoot+"crw-dev/SKILL.md"))...)
		if len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: a MUST USE catch-all, a long description and a neighbour trigger fail", func(t *testing.T) {
		expectEqual(t, "ok", crwDevDescriptionErrors("Classify C0-C5 and load a crw-dev-* owner."), []string(nil))
		got := strings.Join(crwDevDescriptionErrors("MUST USE for coding, PR creation/review/merge, and QA. C0-C5, crw-dev-*."), "\n")
		for _, want := range []string{"claims MUST USE", "neighbour trigger review", "neighbour trigger merge", "neighbour trigger QA", "neighbour trigger PR creation"} {
			if !strings.Contains(got, want) {
				t.Errorf("no error contains %q:\n%s", want, got)
			}
		}
		expectEqual(t, "no router words", len(crwDevDescriptionErrors("Develop code.")), 2)
		expectEqual(t, "long", len(catchAllErrors("x", strings.Repeat("y", 301))), 1)
	})
	t.Run("fixture: the executor row names crw-dev-* skills only", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"crw-dev", "crw-dev-backend"} {
			path := filepath.Join(root, devSkillsRoot, name, "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		table := func(row string) string {
			return "| Caller | Loads | Does not load |\n|---|---|---|\n" + row + "\n"
		}
		expectEqual(t, "ok", executorRowErrors(root, table("| executor of a flow-managed run | `crw-dev`, `crw-dev-backend` | the loop |")), []string(nil))
		got := strings.Join(executorRowErrors(root, table("| executor of a run | `crw-dev`, `crw-loop`, `crw-pabcd`, `crw-dev-nothing` | - |")), "\n")
		for _, want := range []string{"loads crw-loop, which is not a crw-dev-* skill", "loads crw-pabcd, which is not", "names crw-dev-nothing, which has no SKILL.md"} {
			if !strings.Contains(got, want) {
				t.Errorf("no error contains %q:\n%s", want, got)
			}
		}
		expectEqual(t, "no row", executorRowErrors(root, "| Caller | Loads |\n|---|---|\n| someone | `crw-dev` |\n"), []string{"crw-dev: the router has 0 executor rows, want 1"})
		expectEqual(t, "no crw-dev", len(executorRowErrors(root, table("| executor | `crw-dev-backend` | - |"))), 1)
	})
}

// c4RowErrors reads the C4 row of the work classifier: in a flow-managed run the high-risk lane is the C4
// process, with its design audit before any executor, and the executor starts no FSM. The paragraph after the
// table says reviewer-final does not replace the design audit.
func c4RowErrors(text string) []string {
	var process string
	for _, row := range tableRows(text, "Class") {
		if len(row) >= 4 && row[0] == "C4" {
			process = row[3]
		}
	}
	if process == "" {
		return []string{"crw-dev: the classifier has no C4 row"}
	}
	var errs []string
	for _, want := range []string{
		"flow-managed run", "high-risk lane", "architect DESIGN", "independent pre-build design audit",
		"different model family", "before any executor starts", "per-issue candidate review", "reviewer-final", "starts no FSM",
	} {
		if !strings.Contains(process, want) {
			errs = append(errs, "crw-dev C4 row lacks "+want)
		}
	}
	flat := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{"reviewer-final does not replace the pre-build design audit", "oversized child start no PABCD state machine"} {
		if !strings.Contains(flat, want) {
			errs = append(errs, "crw-dev lacks "+want)
		}
	}
	return errs
}

func TestCrwDevC4RowNamesFlowLane(t *testing.T) {
	t.Run("the checked-in skill", func(t *testing.T) {
		if errs := c4RowErrors(skillText(t, repoRoot(), devSkillsRoot+"crw-dev/SKILL.md")); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: the old C4 row fails", func(t *testing.T) {
		old := "| Class | Name | Signals | Default Process |\n|---|---|---|---|\n| C4 | High-Risk | Auth | Full PABCD (mandatory) + full relevant gates + durable risk/evidence record |\n"
		got := strings.Join(c4RowErrors(old), "\n")
		for _, want := range []string{"lacks flow-managed run", "lacks independent pre-build design audit", "lacks starts no FSM", "lacks reviewer-final does not replace"} {
			if !strings.Contains(got, want) {
				t.Errorf("no error contains %q:\n%s", want, got)
			}
		}
		expectEqual(t, "no C4 row", c4RowErrors("| Class | Name | Signals | Default Process |\n|---|---|---|---|\n| C3 | x | y | z |\n"), []string{"crw-dev: the classifier has no C4 row"})
	})
}

// layerWordingErrors is the wording a reviewer or debugging skill must not carry: the old parent semantics
// ("the parent session") and an escalation that names no level.
func layerWordingErrors(file, text string) []string {
	var errs []string
	for number, line := range lines(text) {
		for _, old := range []*regexp.Regexp{regexp.MustCompile(`(?i)\bparent session\b`), regexp.MustCompile(`(?i)\bneeds human judgment\b`), regexp.MustCompile(`(?i)\bdiscuss with the user before\b`)} {
			if old.MatchString(line) {
				errs = append(errs, file+":"+strconv.Itoa(number+1)+": old wording "+old.String()+": "+strings.TrimSpace(line))
			}
		}
	}
	return errs
}

func TestCodeReviewerNoParentSessionWording(t *testing.T) {
	t.Run("the checked-in skills", func(t *testing.T) {
		root := repoRoot()
		var errs []string
		for _, dir := range reviewerWordingReads {
			files, err := scanFiles(root, dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				errs = append(errs, layerWordingErrors(file, skillText(t, root, file))...)
			}
		}
		debugging := skillText(t, root, devSkillsRoot+"crw-dev-debugging/SKILL.md")
		for _, level := range []string{"project orchestrator", "issue child"} {
			if !strings.Contains(debugging, level) {
				errs = append(errs, "crw-dev-debugging: the escalation does not name the level "+level)
			}
		}
		if strings.Contains(debugging, "Logic Analysis") {
			errs = append(errs, "crw-dev-debugging: Logic Analysis is system behaviour analysis")
		}
		if !strings.Contains(debugging, "## System behaviour analysis") {
			errs = append(errs, "crw-dev-debugging: no System behaviour analysis section")
		}
		// The author side of review is crw-dev's; the reviewer holds the independent procedure.
		reviewer := skillText(t, root, devSkillsRoot+"crw-dev-code-reviewer/SKILL.md")
		if strings.Contains(reviewer, "Receiving Code Review") || strings.Contains(reviewer, "Requesting Code Review") {
			errs = append(errs, "crw-dev-code-reviewer: the author side of review belongs to crw-dev")
		}
		if !strings.Contains(skillText(t, root, devSkillsRoot+"crw-dev/SKILL.md"), "## 4.1 Requesting and Receiving Review") {
			errs = append(errs, "crw-dev: no Requesting and Receiving Review section")
		}
		if len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: the old wording is found by line", func(t *testing.T) {
		got := layerWordingErrors("f.md", "ok\nwhich branch the parent session owns\n- Repeated fix attempts failed — needs human judgment\nDiscuss with the user before more fixes.\n")
		expectEqual(t, "count", len(got), 3)
		if !strings.Contains(got[0], "f.md:2:") || !strings.Contains(got[1], "f.md:3:") || !strings.Contains(got[2], "f.md:4:") {
			t.Errorf("lines: %q", got)
		}
		expectEqual(t, "clean", layerWordingErrors("f.md", "the task that dispatched it\n"), []string(nil))
	})
}

// worktreeOwners is the one owner of each kind of worktree, with the skill file its rules are linked from.
var worktreeOwners = map[string]string{
	"Codex-app managed worktree": "crw-worktree-guardian",
	"Branch hygiene":             "crw-dev-devops",
	"The workspace of a run":     "crw-run",
}

var ownerRowLink = regexp.MustCompile(`\]\(([^)#\s]*)(?:#([^)\s]*))?\)`)

// worktreeOwnerErrors reads the owner table of crw-worktree-guardian: three kinds, each with the owner of
// worktreeOwners, and every link of a row names an existing file and an anchor of it. The owners that are not the
// guardian carry no worktree trigger in their descriptions, and crw-run names the other two owners.
func worktreeOwnerErrors(root string, descriptions map[string]string) []string {
	var errs []string
	guardianFile := worktreeOwnerReads[0]
	guardian := skillText0(root, guardianFile)
	rows := tableRows(guardian, "Worktree kind")
	if len(rows) != len(worktreeOwners) {
		errs = append(errs, guardianFile+": the owner table has "+strconv.Itoa(len(rows))+" rows, want "+strconv.Itoa(len(worktreeOwners)))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) < 3 {
			errs = append(errs, guardianFile+": a short owner row")
			continue
		}
		matched := false
		for kind, owner := range worktreeOwners {
			if !strings.HasPrefix(row[0], kind) {
				continue
			}
			matched = true
			seen[kind] = true
			if row[1] != "`"+owner+"`" {
				errs = append(errs, guardianFile+": "+kind+" is owned by "+row[1]+", want "+owner)
			}
		}
		if !matched {
			errs = append(errs, guardianFile+": the owner row "+row[0]+" is no known kind")
		}
		for _, m := range ownerRowLink.FindAllStringSubmatch(row[2], -1) {
			target := filepath.ToSlash(filepath.Join(filepath.Dir(guardianFile), m[1]))
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target)))
			if err != nil {
				errs = append(errs, guardianFile+": the owner link "+m[1]+" does not resolve")
				continue
			}
			if m[2] != "" && !markdownAnchors(string(data))[m[2]] {
				errs = append(errs, guardianFile+": the owner link "+m[1]+" has no anchor #"+m[2])
			}
		}
	}
	for kind := range worktreeOwners {
		if !seen[kind] {
			errs = append(errs, guardianFile+": no owner row for "+kind)
		}
	}
	for _, name := range []string{"crw-dev-devops", "crw-run"} {
		if strings.Contains(strings.ToLower(descriptions[name]), "worktree") {
			errs = append(errs, name+": the description carries a worktree trigger, which belongs to crw-worktree-guardian")
		}
	}
	if !strings.Contains(descriptions["crw-worktree-guardian"], "Codex-app managed worktrees") {
		errs = append(errs, "crw-worktree-guardian: the description is not scoped to Codex-app managed worktrees")
	}
	run := skillText0(root, worktreeOwnerReads[2])
	for _, want := range []string{"`crw-worktree-guardian`", "`crw-dev-devops`"} {
		if !strings.Contains(run, want) {
			errs = append(errs, "crw-run: the workspace section does not name "+want)
		}
	}
	return errs
}

// skillText0 is the file's text, or "" when it cannot be read (the callers report the missing content).
func skillText0(root, rel string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestWorktreeOwnerTable(t *testing.T) {
	t.Run("the checked-in skills", func(t *testing.T) {
		root := repoRoot()
		descriptions, err := skillDescriptions(root)
		if err != nil {
			t.Fatal(err)
		}
		if errs := worktreeOwnerErrors(root, descriptions); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: a wrong owner, a dead link and a worktree trigger fail", func(t *testing.T) {
		root := t.TempDir()
		write := func(rel, text string) {
			path := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write(devSkillsRoot+"crw-worktree-guardian/SKILL.md", "| Worktree kind | Owner | Rules |\n|---|---|---|\n"+
			"| Codex-app managed worktree: one per thread | `crw-worktree-guardian` | this skill |\n"+
			"| Branch hygiene: stale branches | `crw-run` | [lifecycle](../crw-dev-devops/references/branch-lifecycle.md#nowhere) and [gc](../crw-dev-devops/references/local-gc.md) |\n")
		write(devSkillsRoot+"crw-dev-devops/references/branch-lifecycle.md", "# Branch lifecycle\n")
		write(devSkillsRoot+"crw-run/SKILL.md", "workspace\n")
		got := strings.Join(worktreeOwnerErrors(root, map[string]string{"crw-dev-devops": "Clean worktree cleanup", "crw-run": "Run", "crw-worktree-guardian": "Any"}), "\n")
		for _, want := range []string{
			"the owner table has 2 rows, want 3",
			"Branch hygiene is owned by `crw-run`, want crw-dev-devops",
			"has no anchor #nowhere",
			"the owner link ../crw-dev-devops/references/local-gc.md does not resolve",
			"no owner row for The workspace of a run",
			"crw-dev-devops: the description carries a worktree trigger",
			"not scoped to Codex-app managed worktrees",
			"crw-run: the workspace section does not name `crw-worktree-guardian`",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("no error contains %q:\n%s", want, got)
			}
		}
	})
}

// lunasearchOldPathAllowed are the tracked files that keep the name crw-lunasearch on purpose: the rename table of
// the CXC port, the recorded contract corpus and fuzz targets that replay the source behaviour, the port oracle's
// folder allowlist, the skillport records (they quote the original lines of the edits they account for), and the
// notes that quote a past lane's task text. No skill, inventory row, test registration or
// document points at the old path.
var lunasearchOldPathAllowed = []string{
	"contract/",
	"docs/port-cxc/",
	"internal/dev/cxcfuzz/",
	"port/cxc/records/",
	"internal/role/spawn/inline.go",
	"internal/dev/ci/dev_skills_test.go",
	"testdata/flow/parity/",
}

// lunasearchErrors is the state of the merge of crw-lunasearch into crw-search: the directory is gone, the
// reference uses the explorer role key and carries a one-link attachment section, crw-search names the lane in
// its description and links the reference, and no tracked file points at the old path.
func lunasearchErrors(root string) []string {
	var errs []string
	if _, err := os.Stat(filepath.Join(root, devSkillsRoot, "crw-lunasearch")); err == nil {
		errs = append(errs, "plugins/crw/skills/crw-lunasearch: the directory is merged into crw-search")
	}
	if _, err := os.Stat(filepath.Join(root, "port/cxc/records/crw-lunasearch.json")); err == nil {
		errs = append(errs, "port/cxc/records/crw-lunasearch.json: the record left with the skill")
	}
	refPath := devSkillsRoot + "crw-search/references/lunasearch.md"
	ref := skillText0(root, refPath)
	if ref == "" {
		return append(errs, refPath+": missing")
	}
	if !strings.Contains(ref, "`explorer` role key") {
		errs = append(errs, refPath+": does not spawn through the explorer role key")
	}
	for _, hard := range []string{"agent_type", "gpt-5.6-luna", "catalog_list", "reasoning_effort:"} {
		if strings.Contains(ref, hard) {
			errs = append(errs, refPath+": hard-codes "+hard)
		}
	}
	var section []string
	in := false
	for _, line := range lines(ref) {
		if strings.HasPrefix(line, "## ") {
			in = line == "## Subagent Skill Attachment"
			continue
		}
		if in {
			section = append(section, line)
		}
	}
	body := strings.Join(section, "\n")
	if len(section) == 0 {
		errs = append(errs, refPath+": no Subagent Skill Attachment section")
	}
	if links := strings.Count(body, "](../SKILL.md#subagent-skill-attachment-search-attach-01)"); links != 1 {
		errs = append(errs, refPath+": the attachment section has "+strconv.Itoa(links)+" links to SEARCH-ATTACH-01, want 1")
	}
	for _, repeated := range []string{"web_search", "CRW-SKILL-AFFORDANCE", "items:", "Tier 1"} {
		if strings.Contains(body, repeated) {
			errs = append(errs, refPath+": the attachment section repeats "+repeated+", which SEARCH-ATTACH-01 holds")
		}
	}
	search := skillText0(root, devSkillsRoot+"crw-search/SKILL.md")
	if !markdownAnchors(search)["subagent-skill-attachment-search-attach-01"] {
		errs = append(errs, "crw-search/SKILL.md: no SEARCH-ATTACH-01 anchor")
	}
	if n := len(regexp.MustCompile(`(?m)^#+ Subagent Skill Attachment`).FindAllString(search, -1)); n != 1 {
		errs = append(errs, "crw-search/SKILL.md: "+strconv.Itoa(n)+" attachment sections, want 1")
	}
	if !strings.Contains(search, "](references/lunasearch.md)") {
		errs = append(errs, "crw-search/SKILL.md: does not link references/lunasearch.md")
	}
	descriptions, err := skillDescriptions(root)
	if err != nil {
		return append(errs, err.Error())
	}
	if !strings.Contains(descriptions["crw-search"], "Luna") || !strings.Contains(descriptions["crw-search"], "루나검색") {
		errs = append(errs, "crw-search: the description does not name the Luna lane trigger")
	}
	files, err := trackedTextFiles(root)
	if err != nil {
		return append(errs, err.Error())
	}
files:
	for _, name := range files {
		for _, allowed := range lunasearchOldPathAllowed {
			if covers(allowed, name) {
				continue files
			}
		}
		if strings.Contains(skillText0(root, name), "crw-lunasearch") {
			errs = append(errs, name+": points at crw-lunasearch, which is merged into crw-search")
		}
	}
	return errs
}

func TestLunasearchMergedIntoSearch(t *testing.T) {
	t.Run("the checked-in skills", func(t *testing.T) {
		if errs := lunasearchErrors(repoRoot()); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: the old skill, a hard-coded model and a repeated attachment fail", func(t *testing.T) {
		r := newRepo(t)
		r.write(devSkillsRoot+"crw-lunasearch/SKILL.md", "---\nname: crw-lunasearch\ndescription: Luna\n---\n")
		r.write(devSkillsRoot+"crw-search/SKILL.md", "---\nname: crw-search\ndescription: Search\n---\n## Subagent Skill Attachment (SEARCH-ATTACH-01)\n\n## Subagent Skill Attachment (x)\n")
		r.write(devSkillsRoot+"crw-search/references/lunasearch.md", "agent_type: explorer, model gpt-5.6-luna\n\n## Subagent Skill Attachment\n\nUse web_search. [a](../SKILL.md#subagent-skill-attachment-search-attach-01) [b](../SKILL.md#subagent-skill-attachment-search-attach-01)\n")
		r.write("docs/a.md", "see crw-lunasearch\n")
		r.commit()
		got := strings.Join(lunasearchErrors(r.root), "\n")
		for _, want := range []string{
			"crw-lunasearch: the directory is merged into crw-search",
			"does not spawn through the explorer role key",
			"hard-codes agent_type",
			"hard-codes gpt-5.6-luna",
			"the attachment section has 2 links to SEARCH-ATTACH-01, want 1",
			"the attachment section repeats web_search",
			"crw-search/SKILL.md: 2 attachment sections, want 1",
			"does not link references/lunasearch.md",
			"the description does not name the Luna lane trigger",
			"docs/a.md: points at crw-lunasearch",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("no error contains %q:\n%s", want, got)
			}
		}
	})
}

// headingSection is the lines of text from the level-2 heading that starts with prefix to the next level-2
// heading (or the end), or "" when there is none.
func headingSection(text, prefix string) string {
	var section []string
	in := false
	for _, line := range lines(text) {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, prefix)
			continue
		}
		if in {
			section = append(section, line)
		}
	}
	return strings.Join(section, "\n")
}

// governanceRows is the rule rows of a Markdown table whose first cell is a DEVOPS-* rule id, by id.
func governanceRows(text string) map[string]string {
	rows := map[string]string{}
	for _, line := range lines(text) {
		if m := regexp.MustCompile("^\\| `(DEVOPS-[A-Z0-9-]+)` \\|").FindStringSubmatch(line); m != nil {
			rows[m[1]] = line
		}
	}
	return rows
}

var (
	governanceRules = []string{"DEVOPS-REPO-BOOTSTRAP-01", "DEVOPS-AGENT-INTAKE-01"}
	hygieneRules    = []string{"DEVOPS-BRANCH-AUTODELETE-01", "DEVOPS-BRANCH-DELETE-EVIDENCE-01", "DEVOPS-BRANCH-SNAPSHOT-01", "DEVOPS-WORKTREE-DIRTY-01", "DEVOPS-BRANCH-NAMESPACE-01", "DEVOPS-LOCAL-GC-01"}
)

// repoGovernanceErrors compares the rule tables: the two governance rules are rows of crw-canon's
// repo-governance.md only, the six branch, worktree and local-GC rules are rows of crw-dev-devops §2.9 only, the
// owner files of the governance rules are in crw-canon/references only, and no tracked Markdown document points at
// their old place.
func repoGovernanceErrors(root string) []string {
	var errs []string
	devops := governanceRows(headingSection(skillText0(root, devSkillsRoot+"crw-dev-devops/SKILL.md"), "## §2.9 "))
	canon := governanceRows(skillText0(root, devSkillsRoot+"crw-canon/references/repo-governance.md"))
	for _, id := range governanceRules {
		if _, ok := canon[id]; !ok {
			errs = append(errs, "repo-governance.md: no row for "+id)
		}
		if _, ok := devops[id]; ok {
			errs = append(errs, "crw-dev-devops/SKILL.md: keeps the row for "+id+", which belongs to repo-governance.md")
		}
	}
	for _, id := range hygieneRules {
		if _, ok := devops[id]; !ok {
			errs = append(errs, "crw-dev-devops/SKILL.md: no row for "+id)
		}
		if _, ok := canon[id]; ok {
			errs = append(errs, "repo-governance.md: has the row for "+id+", which belongs to crw-dev-devops")
		}
	}
	for id := range canon {
		if !containsString(governanceRules, id) {
			errs = append(errs, "repo-governance.md: the row "+id+" is no governance rule")
		}
	}
	for id := range devops {
		if !containsString(hygieneRules, id) {
			errs = append(errs, "crw-dev-devops/SKILL.md: the row "+id+" is no branch, worktree or local-GC rule")
		}
	}
	for _, id := range governanceRules {
		if row, ok := canon[id]; ok {
			cells := strings.Split(row, "|")
			if len(cells) < 5 || strings.TrimSpace(cells[2]) == "" || strings.TrimSpace(cells[3]) == "" {
				errs = append(errs, "repo-governance.md: the row "+id+" has no severity or statement")
			}
		}
	}
	for _, file := range []string{"repo-bootstrap.md", "agent-pr-intake.md"} {
		if _, err := os.Stat(filepath.Join(root, devSkillsRoot, "crw-canon/references", file)); err != nil {
			errs = append(errs, "crw-canon/references/"+file+": missing")
		}
		if _, err := os.Stat(filepath.Join(root, devSkillsRoot, "crw-dev-devops/references", file)); err == nil {
			errs = append(errs, "crw-dev-devops/references/"+file+": moved to crw-canon/references")
		}
	}
	if !strings.Contains(skillText0(root, devSkillsRoot+"crw-dev-devops/SKILL.md"), "](../crw-canon/references/repo-governance.md)") {
		errs = append(errs, "crw-dev-devops/SKILL.md: does not link repo-governance.md")
	}
	files, err := repositoryFiles(root)
	if err != nil {
		return append(errs, err.Error())
	}
	old := regexp.MustCompile(`crw-dev-devops/references/(repo-bootstrap|agent-pr-intake)\.md`)
	for _, name := range files {
		if filepath.Ext(name) != ".md" || name == "docs/skills/inventory-map.json" {
			continue
		}
		for i, line := range lines(skillText0(root, name)) {
			if m := old.FindString(line); m != "" {
				errs = append(errs, name+":"+strconv.Itoa(i+1)+": points at "+m+", which moved to crw-canon/references")
			}
		}
		if strings.HasPrefix(name, devSkillsRoot+"crw-dev-devops/") {
			for i, line := range lines(skillText0(root, name)) {
				if regexp.MustCompile("`(references/)?(repo-bootstrap|agent-pr-intake)\\.md`").MatchString(line) {
					errs = append(errs, name+":"+strconv.Itoa(i+1)+": names a governance owner file as if it were in crw-dev-devops")
				}
			}
		}
	}
	return errs
}

func containsString(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

func TestRepoGovernanceInCanon(t *testing.T) {
	t.Run("the checked-in skills", func(t *testing.T) {
		root := repoRoot()
		if errs := repoGovernanceErrors(root); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
		for _, read := range governanceReads {
			if files, err := scanFiles(root, read); err != nil || len(files) == 0 {
				t.Errorf("%s: holds no file to read (%v)", read, err)
			}
		}
	})
	t.Run("fixture: governance rows left in devops, a missing canon row and an old path fail", func(t *testing.T) {
		r := newRepo(t)
		rows := func(ids ...string) string {
			text := "| Rule | Severity | Statement |\n|---|---|---|\n"
			for _, id := range ids {
				text += "| `" + id + "` | DEFAULT | a statement |\n"
			}
			return text
		}
		r.write(devSkillsRoot+"crw-dev-devops/SKILL.md", "## §2.9 Branch Lifecycle Hygiene (STRICT)\n\n"+rows("DEVOPS-REPO-BOOTSTRAP-01", "DEVOPS-BRANCH-AUTODELETE-01"))
		r.write(devSkillsRoot+"crw-canon/references/repo-governance.md", rows("DEVOPS-LOCAL-GC-01"))
		r.write(devSkillsRoot+"crw-dev-devops/references/repo-bootstrap.md", "# x\n")
		r.write("docs/a.md", "[b](../plugins/crw/skills/crw-dev-devops/references/agent-pr-intake.md)\n")
		r.commit()
		got := strings.Join(repoGovernanceErrors(r.root), "\n")
		for _, want := range []string{
			"repo-governance.md: no row for DEVOPS-REPO-BOOTSTRAP-01",
			"crw-dev-devops/SKILL.md: keeps the row for DEVOPS-REPO-BOOTSTRAP-01",
			"repo-governance.md: no row for DEVOPS-AGENT-INTAKE-01",
			"repo-governance.md: has the row for DEVOPS-LOCAL-GC-01",
			"crw-dev-devops/SKILL.md: no row for DEVOPS-LOCAL-GC-01",
			"crw-canon/references/repo-bootstrap.md: missing",
			"crw-dev-devops/references/repo-bootstrap.md: moved to crw-canon/references",
			"docs/a.md:1: points at crw-dev-devops/references/agent-pr-intake.md",
			"does not link repo-governance.md",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("no error contains %q:\n%s", want, got)
			}
		}
	})
}
