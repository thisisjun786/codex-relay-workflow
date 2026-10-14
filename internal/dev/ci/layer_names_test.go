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
	"unicode"
	"unicode/utf8"
)

// CRW-1352 (Jun 10-14 04:3x, Sion relaying; design-1316 section 18.1): the session layers are named
// 총괄 (top session), 부모 (parent: the repository session, relay role `supervisor`, the management seat of
// CRW-1314), 자식 (child: the project session, relay role `parent`) and 서브에이전트 (subagents, no role id).
// hierarchy.md holds the one table of those names; the relay role ids, stored fields, flags and CLI words
// stay as they are, so the table warns that the relay's `parent` is the child. The four old layer names
// leave the 1.0-path skills and documents; the records (known-defects, port records, old handoffs) stay as
// written. The dispatch case files under docs/crw-run/dispatch-cases are the source of a skill reference, not records. crw-run-initiative, the initiative lead's skill, leaves the 1.0
// path and the parent's work is folded into crw-run.

// oldLayerName is a layer name the table replaces, in English and in Korean, and (CRW-1353, the CRW-1352 carry-over d2) the
// stand-ins that replaced them outside the table: the orchestrator (thread) for the child, the manager for the parent, and the
// initiative's supervisor for the parent.
var oldLayerName = regexp.MustCompile(`(?i)management session|initiative lead|project orchestrator|project owner|관리 세션|이니셔티브 리드|프로젝트 오케스트레이터|프로젝트 담당|` +
	`orchestrator thread|\bthe orchestrator\b|\bthe manager\b|initiative's supervisor|initiative supervisor`)

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
		stand := oldLayerNameProblems(map[string]string{"a.md": "the orchestrator thread\nThe orchestrator decides\nfrom the manager down\nthe initiative's supervisor\nan initiative supervisor\na workflow orchestrator binary\n"})
		if len(stand) != 5 {
			t.Errorf("stand-ins = %v, want five (an orchestrator that is no layer stays)", stand)
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
	body := normalized(withoutFrontmatter(text))
	masked := normalized(maskCode(withoutFrontmatter(text)))
	problems = append(problems, bareLayerWordProblems(rel, body, masked)...)
	if (uses || bareLayerWordUsed(rel, masked)) && !regexp.MustCompile(`\]\([^)]*hierarchy\.md#the-levels\)`).MatchString(text) {
		problems = append(problems, rel+": names a layer and does not link hierarchy.md#the-levels")
	}
	return problems
}

// CRW-1396: a document that names a layer only as "the parent" or "the child" escaped both checks above,
// which look for the labels "repository session" and "project session". The words are also the relay role
// ids, so the table's rule tells the two senses apart at the first use: the layer sense gives its label and
// role id (the parent (repository session, relay role `supervisor`), the child (project session, relay role
// `parent`)), and relay-role prose keeps its meaning and says so (the parent (relay role `parent`), the child
// (relay role `child`)). Either way the first use of each word gives a relay role id and the file links the
// table. A case file under docs/crw-run/dispatch-cases is a fragment of crw-run's dispatch verification
// reference, which is checked as a whole, and a JSON or YAML file holds metadata that links nothing.

// bareLayerWord is "the parent" or "the child" as a noun of its own, not a compound such as the child role or
// the child pair line.
var bareLayerWord = regexp.MustCompile(`(?i)\bthe (parent|child)\b`)

// layerWordCompound is what may follow the word without making it a layer or a role: a relay role or the child
// pair line named as such, a hyphenated compound, and the parent or child of a plan revision, a path, a process,
// a test or a turn.
var layerWordCompound = regexp.MustCompile(`^(?:['’]s)? ?(?:(?:role|roles|pair|pairs|plan|path|process|test|turn)\b|-)`)

// nonLayerUses are phrases where the word means a git parent, a process, a design term or the session and the
// subagent of a native spawn, read literally.
var nonLayerUses = []string{
	"no longer the parent refuses the commit",
	"at the parent of the commit",
	"the parent render chain",
	"the parent direction",
	"the parent RUN finished",
	"requires the child to exit",
	"the supervisor restarts the child",
	"reads the parent as context",
	"State the child's question",
	"asking the child to self-load",
	"the child's first turn parses the mention",
	"Do not ask the parent or the user to recap",
	"the child's start time",
}

// nonSessionFiles are files whose parent and child are never a session layer or a relay role, with what they are.
var nonSessionFiles = map[string]string{
	"docs/flow/journal.md":                                  "a git parent commit and the parent path of a nested call",
	"docs/relay/dag-plans.md":                               "the parent revision of a plan and the Linear parent of an issue",
	"plugins/crw/skills/crw-dev/references/stacked-prs.md":  "the parent and child branches of a stacked pull request, and the session a native subagent runs in",
	"plugins/crw/skills/crw-pabcd/references/delegation.md": "the session that delegates to a native subagent and that subagent",
	"plugins/crw/skills/crw-loop/references/waiting.md":     "a native subagent and the session that waits on it",
}

// layerWordSenses are the openings a first use's parenthetical may take, per word.
var layerWordSenses = map[string][]string{
	"parent": {"repository session, relay role `supervisor`", "relay role `parent`"},
	"child":  {"project session, relay role `parent`", "relay role `child`"},
}

func bareLayerWordChecked(rel string) bool {
	_, nonSession := nonSessionFiles[rel]
	return strings.HasSuffix(rel, ".md") && !strings.HasPrefix(rel, "docs/crw-run/dispatch-cases/") && !nonSession
}

// withoutFrontmatter is text without a leading YAML front matter block: a skill's description is metadata that
// links nothing, and the body gives the first use.
func withoutFrontmatter(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return text
	}
	if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
		return text[4+i+5:]
	}
	return text
}

// nonLayerUse matches any phrase of nonLayerUses in any case, as bareLayerWord matches the word, so a phrase that opens
// a sentence is blanked too (Argon V5 K3).
var nonLayerUse = func() *regexp.Regexp {
	quoted := make([]string, len(nonLayerUses))
	for i, phrase := range nonLayerUses {
		quoted[i] = regexp.QuoteMeta(phrase)
	}
	return regexp.MustCompile(`(?i)` + strings.Join(quoted, "|"))
}()

// codeToMask is a fenced block (``` or ~~~) or an inline code span: a command, a schema key or an example,
// where "the parent" is quoted text and names no session (CRW-1402).
var codeToMask = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`\n]*`")

// maskCode is text with the code blanked, character for character: every non-space character of a code span
// or fenced block becomes "_" and the spaces and line breaks stay, so normalized(maskCode(text)) and
// normalized(text) are the same number of bytes with every offset in the same place. The first use is looked for in
// the masked text and its role id is read from the unmasked one, which keeps the code span `supervisor`
// of "relay role `supervisor`" intact.
func maskCode(text string) string {
	return codeToMask.ReplaceAllStringFunc(text, func(code string) string {
		var b strings.Builder
		for _, r := range code {
			if unicode.IsSpace(r) {
				b.WriteRune(r)
			} else {
				b.WriteString(strings.Repeat("_", utf8.RuneLen(r)))
			}
		}
		return b.String()
	})
}

// bareLayerWordFirsts is the offset of the first bare use of each word in masked, the text with its code blanked
// (maskCode), after the literal non-layer phrases are blanked, whatever their case; a blank keeps the length of
// what it covers.
func bareLayerWordFirsts(flatText string) map[string]int {
	flatText = nonLayerUse.ReplaceAllStringFunc(flatText, func(match string) string { return strings.Repeat("_", len(match)) })
	firsts := map[string]int{}
	for _, m := range bareLayerWord.FindAllStringSubmatchIndex(flatText, -1) {
		word := strings.ToLower(flatText[m[2]:m[3]])
		if _, seen := firsts[word]; seen {
			continue
		}
		if layerWordCompound.MatchString(flatText[m[1]:]) {
			continue
		}
		firsts[word] = m[1]
	}
	return firsts
}

func bareLayerWordUsed(rel, flatText string) bool {
	return bareLayerWordChecked(rel) && len(bareLayerWordFirsts(flatText)) > 0
}

var linkTarget = regexp.MustCompile(`^\]\([^)]*\)`)

// bareLayerWordProblems looks for the first bare use in masked and reads what follows it in flatText.
func bareLayerWordProblems(rel, flatText, masked string) []string {
	if !bareLayerWordChecked(rel) {
		return nil
	}
	var problems []string
	for _, word := range []string{"parent", "child"} {
		end, ok := bareLayerWordFirsts(masked)[word]
		if !ok {
			continue
		}
		after := linkTarget.ReplaceAllString(flatText[end:], "")
		given := false
		if strings.HasPrefix(after, " (") {
			for _, sense := range layerWordSenses[word] {
				given = given || strings.HasPrefix(after[2:], sense)
			}
		}
		if !given {
			problems = append(problems, rel+": the first \"the "+word+"\" gives no relay role id ("+strings.Join(layerWordSenses[word], " or ")+"): ..."+flatText[max(0, end-len("the "+word)-30):min(len(flatText), end+60)])
		}
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
	t.Run("fixture: the parent and the child used without the first-use role id (CRW-1396)", func(t *testing.T) {
		link := " See [the level table](x/hierarchy.md#the-levels)."
		for _, good := range []string{
			"The parent (repository session, relay role `supervisor`) decides. The child (project session, relay role `parent`) runs it." + link,
			"[The parent](x/hierarchy.md#the-levels) (repository session, relay role `supervisor`) decides, and the parent's record holds it.",
			"The parent (relay role `parent`) accepts what the child (relay role `child`) reports." + link,
			"The child role and the child pair line, the parent-title replay, a HEAD that is no longer the parent refuses the commit.",
			// Argon V5 K3: a phrase that is not a session reads the same at the start of a sentence or in another case.
			"The parent render chain draws it. First, state the child's question. The Parent Direction holds.",
		} {
			if p := layerLinkProblems("docs/a.md", good); len(p) != 0 {
				t.Errorf("good %q: %v", good, p)
			}
		}
		// The old forms of the documents CRW-1396 names: each is caught, with the missing link.
		for _, old := range []struct {
			rel, text string
		}{
			{"docs/relay/risk-tier.md", "The decision of the parent (10-11) reshapes it. The parent may record an exception. It is decided by Jun and the parent."},
			{"docs/flow/session-host.md", "A session call (CRW-1316) runs a role call as a subagent the child spawns with `spawn_agent` (multi-agent V1)."},
			{"docs/CI.md", "The parent changes the branch protection when the push-only procedure is switched on."},
			{"POLICY.md", "approval scope (Jun, 2026-10-06: Node for the build and CI only), the parent chose this layout"},
		} {
			if p := layerLinkProblems(old.rel, normalized(old.text)); len(p) != 2 {
				t.Errorf("old %s: %v, want the first use and the link", old.rel, p)
			}
		}
		if p := layerLinkProblems("docs/crw-run/dispatch-cases/recorded/0110-S9c.md", "| S9c | The parent issues no creation |"); len(p) != 0 {
			t.Errorf("a dispatch case fragment is checked as part of its document: %v", p)
		}
	})
	t.Run("fixture: code spans and fenced blocks hold no layer word (CRW-1402)", func(t *testing.T) {
		link := " See [the level table](x/hierarchy.md#the-levels)."
		prose := "The parent (repository session, relay role `supervisor`) decides." + link
		for name, good := range map[string]string{
			"a fenced block before the prose":      "```\nthe parent runs the child\n```\n" + prose,
			"a fenced block with a language":       "```sh\ncrw say the child\n```\n" + prose,
			"an inline span before the prose":      "Run `the parent` first. " + prose,
			"code only, so no layer and no link":   "Run `the parent` and `the child`.\n\n```\nthe child\n```\n",
			"multibyte text before the span":       "가나다 `the parent` 라마바. " + prose,
			"the role id after the span is intact": "The child (project session, relay role `parent`) runs `the parent` for it." + link,
		} {
			if p := layerLinkProblems("docs/a.md", good); len(p) != 0 {
				t.Errorf("%s: %v", name, p)
			}
		}
		// Code does not hide a prose use: the first bare use in the prose still needs its role id and the link.
		for name, bad := range map[string]string{
			"a fenced block, then a bare prose use": "```\nthe parent\n```\nThe parent decides.",
			"an inline span, then a bare prose use": "Run `the child`. The child decides.",
		} {
			if p := layerLinkProblems("docs/a.md", bad); len(p) != 2 {
				t.Errorf("%s: %v, want the first use and the link", name, p)
			}
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

// TestRunSkillUsesTheLayerNames (CRW-1353, the CRW-1352 carry-over d1): crw-run's own text uses the table's names, so its relay
// sense of "parent" (this task in its relay role `parent`) is written as the child and "child" as the issue child. The parent
// (repository session) has its own section at the end of the skill; anywhere before it, outside code spans and link texts, each
// "parent" is the repository session and says so: "the parent (repository session...". The relay role id `parent` stays a word in
// code font. The whole file is read (Argon V5 K6): inside the parent's section each "parent" is that section's parent, "the
// parent" or "the repository's parent", and nowhere does the skill give a word its relay sense.
func TestRunSkillUsesTheLayerNames(t *testing.T) {
	text := readRepoText(t, runSkillFile)
	if !strings.Contains(text, parentSectionAt) {
		t.Errorf("%s has no section %q", runSkillFile, strings.TrimSpace(parentSectionAt))
	}
	for _, p := range runSkillParentProblems(text) {
		t.Error(p)
	}
	t.Run("fixture: the relay sense of parent is caught", func(t *testing.T) {
		good := "The child (project session, relay role `parent`) reports to [the parent](h.md#the-levels) (repository session, relay role `supervisor`). " +
			"See [Project parent binding](i.md#project-parent-binding) and `crw manage send-parent`.\n\n## The parent: the repository session\n\n" +
			"The parent orders the projects. Everywhere else in this skill the task is the child, and the parent is always this repository session; " +
			"the parent's record holds it, and it is the repository's parent.\n"
		if p := runSkillParentProblems(good); len(p) != 0 {
			t.Errorf("good: %v", p)
		}
		if p := runSkillParentProblems("The parent dispatches a child for each issue, and a parent goal waits.\n"); len(p) != 2 {
			t.Errorf("relay sense: %v", p)
		}
		// CRW-1402: the word after the parent is read too, so a relay-sense compound in the parent's section is caught.
		section := "Intro.\n## The parent: the repository session\n\n"
		for _, bad := range []string{
			"The parent goal is set when the child starts.\n",
			"It reads the parent binding of the thread.\n",
			"Where the parent role is given, the repository's parent thread is named.\n",
		} {
			if p := runSkillParentProblems(section + bad); len(p) == 0 {
				t.Errorf("a relay-sense compound passes in the parent's section: %q", bad)
			}
		}
		for _, good := range []string{
			"The parent decides, and the parent's goal is the repository's.\n",
			"The parent orders the projects; the repository's parent (not a child) holds the goal.\n",
		} {
			if p := runSkillParentProblems(section + good); len(p) != 0 {
				t.Errorf("good %q: %v", good, p)
			}
		}
		// The sentence K6 found in the parent's own section: a parent that is no section's parent and a relay sense.
		if p := runSkillParentProblems("Intro.\n## The parent: the repository session\n\nEverywhere else in this skill the task is a child and the word parent keeps its relay sense.\n"); len(p) != 2 {
			t.Errorf("relay sense in the parent's section: %v", p)
		}
	})
}

// relayCompound is a noun that follows "parent" in its relay sense (the parent goal, the parent binding, the parent role):
// in the parent's section, "the parent" is the repository session, which has no such compound (CRW-1402).
var relayCompound = regexp.MustCompile(`(?i)^ (?:goals?|bindings?|roles?|threads?|tasks?|ids?|keys?|scopes?|reports?|packets?|pairs?|lines?)\b`)

var (
	parentWord      = regexp.MustCompile(`(?i)\bparents?\b`)
	runSkillLink    = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	parentLabelled  = regexp.MustCompile(`^(\]\([^)]*\))? \(repository session`)
	sectionParent   = regexp.MustCompile(`(?i)\b(the|repository's) $`)
	relaySense      = regexp.MustCompile(`(?i)\brelay sense\b`)
	parentSectionAt = "\n## The parent: the repository session"
)

// runSkillProse is crw-run's text as prose: without front matter and code, a link whose text is the parent keeping its target, so
// the label after it is still seen, and any other link text blanked, since it is a heading's name.
func runSkillProse(text string) string {
	body := codeSpans.ReplaceAllString(withoutFrontmatter(text), "``")
	body = runSkillLink.ReplaceAllStringFunc(body, func(link string) string {
		if regexp.MustCompile(`(?i)^\[the parent\]`).MatchString(link) {
			return "the parent" + link[strings.Index(link, "]"):]
		}
		return "[]"
	})
	return normalized(body)
}

// runSkillParentProblems are the uses of "parent" in crw-run's text that do not name the repository session: before the parent's
// own section each one says so, and inside it each one is the section's parent; and any sentence that gives a word its relay sense.
func runSkillParentProblems(text string) []string {
	before, section, _ := strings.Cut(text, parentSectionAt)
	var problems []string
	flat := runSkillProse(before)
	for _, m := range parentWord.FindAllStringIndex(flat, -1) {
		if !parentLabelled.MatchString(flat[m[1]:]) {
			problems = append(problems, "crw-run: a parent that is not the repository session: ..."+flat[max(0, m[0]-60):min(len(flat), m[1]+40)])
		}
	}
	if section != "" {
		inSection := runSkillProse(strings.TrimSpace(parentSectionAt) + section)
		for _, m := range parentWord.FindAllStringIndex(inSection, -1) {
			if !sectionParent.MatchString(inSection[max(0, m[0]-len("repository's ")):m[0]]) {
				problems = append(problems, "crw-run: a parent in the parent's section that is not the section's parent: ..."+inSection[max(0, m[0]-60):min(len(inSection), m[1]+40)])
			} else if relayCompound.MatchString(inSection[m[1]:]) {
				problems = append(problems, "crw-run: a parent in the parent's section that is a relay-sense compound, not the repository session: ..."+inSection[max(0, m[0]-60):min(len(inSection), m[1]+40)])
			}
		}
		flat += " " + inSection
	}
	for _, m := range relaySense.FindAllStringIndex(flat, -1) {
		problems = append(problems, "crw-run: a word keeps its relay sense, while the skill names every session by the level table: ..."+flat[max(0, m[0]-80):min(len(flat), m[1]+20)])
	}
	return problems
}
