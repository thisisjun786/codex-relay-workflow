//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// The reference-only skill crw-canon holds the sections that more than one skill reads. These tests
// prove where they live, that nothing else repeats them, that no consumer in the repository still
// points at their old place, and the contents that make the library usable: the level table, the
// role keys and the never-implicit skill.

const (
	canonSkillDir = "plugins/crw/skills/crw-canon/"
	canonRefDir   = canonSkillDir + "references/"
	planIntegrate = "plugins/crw/skills/crw-plan/references/integrations.md"
)

// canonSection is one heading that lives in a file of crw-canon/references.
type canonSection struct{ File, Heading string }

// canonMoved is every heading that moved out of crw-plan/references (integrations.md and
// design-explanation.md) into crw-canon/references. The title of design-explanation.md stands for
// the whole file; its other headings are ordinary words and are not scanned for.
var canonMoved = []canonSection{
	{"hierarchy.md", "Scope of each level"},
	{"hierarchy.md", "The message both relations are read by"},
	{"hierarchy.md", "Direct coordination between children"},
	{"release-gate.md", "The release gate"},
	{"schedule-contract.md", "Schedule baseline contract"},
	{"linear-model.md", "Linear operating model"},
	{"linear-model.md", "Issue-to-PR mapping"},
	{"linear-model.md", "Record writes and returned proposals"},
	{"linear-model.md", "Resolve the implementation repository"},
	{"linear-model.md", "Implementation Done"},
	{"linear-model.md", "Linear holds canonical documents"},
	{"linear-model.md", "Where each document type is canonical"},
	{"linear-model.md", "Classify an existing document"},
	{"linear-model.md", "Keep the two sides consistent"},
	{"linear-model.md", "When the implementing repository changes"},
	{"linear-model.md", "Initiative body standard"},
	{"linear-model.md", "Use the available Linear capability"},
	{"delivery.md", "Workflow ownership"},
	{"delivery.md", "Completion follow-up in an existing execution workflow"},
	{"delivery.md", "Default independent execution"},
	{"delivery.md", "Default parent start policy"},
	{"delivery.md", "Publish for review when the work is reviewable"},
	{"delivery.md", "Default dev integration"},
	{"delivery.md", "Whether a relay holds this assignment"},
	{"delivery.md", "Durable cross-task delivery"},
	{"delivery.md", "Installation and operations have one owner"},
	{"delivery.md", "Review evidence is independent of the provider"},
	{"delivery.md", "Evidence and handoff"},
	{"delivery.md", "Delivery reach and current usability"},
	{"execution-roles.md", "Execution settings by role"},
	{"design-explanation.md", "Explaining a design"},
	{"skill-family.md", "Skill names under each installation"},
	{"skill-family.md", "Family invariants"},
}

// familyInvariantIDs are the rules crw-canon/references/skill-family.md defines for every skill; a definition
// is the rule id in parentheses closing a bold label.
var familyInvariantIDs = []string{"FAMILY-SLOP-01", "FAMILY-READER-01", "FAMILY-CITE-01", "FAMILY-PROOF-01"}

const crwDevSkill = "plugins/crw/skills/crw-dev/SKILL.md"

// crwDevStubErrors: crw-dev keeps no stub section and no copy of the family invariants; each invariant is
// defined once, in crw-canon's skill-family.md, and crw-dev links to that section.
func crwDevStubErrors(root string) []string {
	var errs []string
	data, err := os.ReadFile(filepath.Join(root, crwDevSkill))
	if err != nil {
		return []string{crwDevSkill + ": " + err.Error()}
	}
	for _, h := range headingsOf(string(data)) {
		if strings.HasSuffix(h, "(stub)") || strings.Contains(strings.ToLower(h), "family invariants") {
			errs = append(errs, crwDevSkill+": heading "+h+" is a stub or a copy of crw-canon/references/skill-family.md")
		}
	}
	if !strings.Contains(string(data), "crw-canon/references/skill-family.md#family-invariants") {
		errs = append(errs, crwDevSkill+": does not link crw-canon/references/skill-family.md#family-invariants")
	}
	files, err := repositoryFiles(root)
	if err != nil {
		return append(errs, err.Error())
	}
	for _, id := range familyInvariantIDs {
		definition := "(" + id + ").**"
		count := 0
		for _, name := range files {
			if filepath.Ext(name) != ".md" || strings.HasPrefix(name, "docs/port-cxc/") {
				continue
			}
			text, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				continue
			}
			if n := strings.Count(string(text), definition); n > 0 {
				if name != canonRefDir+"skill-family.md" {
					errs = append(errs, name+": defines "+id+", which crw-canon/references/skill-family.md defines")
				}
				count += n
			}
		}
		if count != 1 {
			errs = append(errs, "crw-canon/references/skill-family.md: "+id+" is defined "+strconv.Itoa(count)+" times, want once")
		}
	}
	return errs
}

// planningOnlyHeadings is every heading crw-plan/references/integrations.md keeps: the sections that
// only planning reads.
var planningOnlyHeadings = []string{
	"Linear and implementation workflow integration",
	"Resolve the project target",
	"Project parent binding",
	"Establish the link",
	"Set the app presentation and record",
	"Continue from the fixed project",
	"Child pair by issue type",
	"Classify the issue by its kind of work",
	"Bundles",
	"The extended line",
	"The record at release",
	"Review the classification and change the table",
	"The place for a quota rule",
}

var atxHeadingLine = regexp.MustCompile("^ {0,3}#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$")

// headingsOf is the ATX heading texts of a Markdown document in order, outside fenced code.
func headingsOf(text string) []string {
	var out []string
	fence := ""
	for _, line := range lines(text) {
		if marker := fenceMarker(strings.TrimLeft(line, " ")); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case marker[0] == fence[0] && len(marker) >= len(fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := atxHeadingLine.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// fenceMarker is the run of three or more backticks or tildes that opens or closes a fence, or "".
func fenceMarker(line string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(line) && line[n] == c {
			n++
		}
		if n >= 3 {
			return line[:n]
		}
	}
	return ""
}

// trackedTextFiles is the repository files of the kinds a pointer into a skill can live in.
func trackedTextFiles(root string) ([]string, error) {
	files, err := repositoryFiles(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range files {
		switch filepath.Ext(name) {
		case ".md", ".go", ".json", ".yaml":
			out = append(out, name)
		}
	}
	return out, nil
}

// sharedSectionOutsideCanonErrors: each moved heading is in its canon file once and in no other
// Markdown document; the planning file keeps exactly its own sections.
func sharedSectionOutsideCanonErrors(root string) []string {
	var errs []string
	files, err := repositoryFiles(root)
	if err != nil {
		return []string{err.Error()}
	}
	moved := map[string]string{}
	for _, s := range canonMoved {
		moved[s.Heading] = s.File
		data, err := os.ReadFile(filepath.Join(root, canonRefDir, s.File))
		if err != nil {
			errs = append(errs, canonRefDir+s.File+": "+err.Error())
			continue
		}
		count := 0
		for _, h := range headingsOf(string(data)) {
			if h == s.Heading {
				count++
			}
		}
		if count != 1 {
			errs = append(errs, canonRefDir+s.File+": heading "+s.Heading+" appears "+strconv.Itoa(count)+" times, want once")
		}
	}
	for _, name := range files {
		if filepath.Ext(name) != ".md" || strings.HasPrefix(name, canonSkillDir) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, h := range headingsOf(string(data)) {
			if file, ok := moved[h]; ok && name != planIntegrate {
				errs = append(errs, name+": heading "+h+" belongs to crw-canon/references/"+file)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, planIntegrate)); err != nil {
		errs = append(errs, planIntegrate+": "+err.Error())
	} else {
		got := headingsOf(string(data))
		for _, h := range got {
			if file, ok := moved[h]; ok {
				errs = append(errs, planIntegrate+": heading "+h+" belongs to crw-canon/references/"+file)
			}
		}
		if !slices.Equal(got, planningOnlyHeadings) {
			errs = append(errs, planIntegrate+": headings "+strings.Join(got, " | ")+"; want only the planning sections "+strings.Join(planningOnlyHeadings, " | "))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "plugins/crw/skills/crw-plan/references/design-explanation.md")); err == nil {
		errs = append(errs, "plugins/crw/skills/crw-plan/references/design-explanation.md: moved to crw-canon/references")
	}
	return append(errs, crwDevStubErrors(root)...)
}

func TestNoSharedSectionOutsideCanon(t *testing.T) {
	t.Run("the checked-in tree", func(t *testing.T) {
		if errs := sharedSectionOutsideCanonErrors(repoRoot()); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: a repeated heading, a missing one and an extra planning section fail", func(t *testing.T) {
		r := newRepo(t)
		for _, s := range canonMoved {
			if s.File == "hierarchy.md" {
				continue
			}
			r.write(canonRefDir+s.File, "# "+s.Heading+"\n")
		}
		r.write(canonRefDir+"hierarchy.md", "# Scope of each level\n\n## Direct coordination between children\n")
		r.write(planIntegrate, "# Linear and implementation workflow integration\n\n## The release gate\n")
		r.write("docs/other.md", "## Direct coordination between children\n")
		r.commit()
		joined := strings.Join(sharedSectionOutsideCanonErrors(r.root), "\n")
		for _, want := range []string{
			"hierarchy.md: heading The message both relations are read by appears 0 times",
			"docs/other.md: heading Direct coordination between children belongs to crw-canon/references/hierarchy.md",
			planIntegrate + ": heading The release gate belongs to crw-canon/references/release-gate.md",
			planIntegrate + ": headings Linear and implementation workflow integration | The release gate; want only the planning sections",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("errors lack %q:\n%s", want, joined)
			}
		}
	})
	t.Run("fixture: a crw-dev stub, a copy of the family invariants and a repeated definition fail", func(t *testing.T) {
		r := newRepo(t)
		r.write(crwDevSkill, "# Dev\n\n## Family Invariants (apply to every crw-* skill)\n\n- **Anti-slop output (FAMILY-SLOP-01).** x\n\n## 6. Code Quality Signals (stub)\n")
		r.write(canonRefDir+"skill-family.md", "# Skill family\n\n## Family invariants\n\n- **Anti-slop output (FAMILY-SLOP-01).** x\n- **Reader (FAMILY-READER-01).** x\n- **Cite (FAMILY-CITE-01).** x\n- **Cite again (FAMILY-CITE-01).** x\n")
		r.write("docs/other.md", "- **Completion proof (FAMILY-PROOF-01).** x\n")
		r.commit()
		joined := strings.Join(crwDevStubErrors(r.root), "\n")
		for _, want := range []string{
			"heading Family Invariants (apply to every crw-* skill) is a stub or a copy",
			"heading 6. Code Quality Signals (stub) is a stub or a copy",
			"does not link crw-canon/references/skill-family.md#family-invariants",
			"FAMILY-CITE-01 is defined 2 times, want once",
			"docs/other.md: defines FAMILY-PROOF-01",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("errors lack %q:\n%s", want, joined)
			}
		}
	})
}

// formerSlugs are the anchors of headings that were renamed on the move, with the canon file that
// holds the section now.
var formerSlugs = []canonSection{
	{"hierarchy.md", "Supervisor, parent and child scope"},
	{"hierarchy.md", "Direct coordination between parents"},
	{"hierarchy.md", "Direct coordination between project orchestrators"},
}

// movedSlugs is the anchor of every moved heading, and of every heading it was renamed from, with the
// canon file that now holds it.
func movedSlugs() (slugs []string, files []string) {
	for _, s := range append(append([]canonSection{}, canonMoved...), formerSlugs...) {
		slugs = append(slugs, anchorSlug(s.Heading))
		files = append(files, s.File)
	}
	return slugs, files
}

var canonLink = regexp.MustCompile("crw-canon/references/([A-Za-z0-9_.-]+\\.md)#([^\\s)>\"'`\\]]+)")

// consumerErrors: no tracked .md, .go, .json or .yaml file points at an anchor that moved out of the
// planning integrations file or at the old place of design-explanation.md, every link into
// crw-canon/references names a heading that exists, and the consumers the issue names carry the new
// location.
func consumerErrors(root string, mustPoint []string) []string {
	files, err := trackedTextFiles(root)
	if err != nil {
		return []string{err.Error()}
	}
	var errs []string
	slugs, canonFiles := movedSlugs()
	var oldLinks, sameFile []*regexp.Regexp
	for _, slug := range slugs {
		oldLinks = append(oldLinks, regexp.MustCompile(`integrations\.md#`+regexp.QuoteMeta(slug)+`(?:[^A-Za-z0-9_-]|$)`))
		sameFile = append(sameFile, regexp.MustCompile(`\(#`+regexp.QuoteMeta(slug)+`\)`))
	}
	anchors := map[string]map[string]bool{}
	for _, name := range files {
		if strings.HasPrefix(name, "internal/dev/ci/") && (strings.HasSuffix(name, "skill_canon_test.go") || strings.HasSuffix(name, "size_rules_test.go")) {
			continue // these tests quote the old places on purpose
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for i, line := range lines(string(data)) {
			where := name + ":" + strconv.Itoa(i+1) + ": "
			for j, re := range oldLinks {
				if re.MatchString(line) {
					errs = append(errs, where+"points at integrations.md#"+slugs[j]+", which moved to crw-canon/references/"+canonFiles[j])
				}
			}
			if name == planIntegrate {
				for j, re := range sameFile {
					if re.MatchString(line) {
						errs = append(errs, where+"links to (#"+slugs[j]+"), which moved to crw-canon/references/"+canonFiles[j])
					}
				}
			}
			if !strings.HasPrefix(name, canonSkillDir) && strings.Contains(line, "design-explanation.md") && !strings.Contains(line, "crw-canon") {
				errs = append(errs, where+"design-explanation.md moved to crw-canon/references")
			}
			for _, m := range canonLink.FindAllStringSubmatch(line, -1) {
				file, slug := m[1], strings.TrimRight(m[2], ".,;:")
				if anchors[file] == nil {
					target, err := os.ReadFile(filepath.Join(root, canonRefDir, file))
					if err != nil {
						errs = append(errs, where+"links to crw-canon/references/"+file+", which cannot be read")
						anchors[file] = map[string]bool{}
						continue
					}
					anchors[file] = markdownAnchors(string(target))
				}
				if !anchors[file][slug] {
					errs = append(errs, where+"crw-canon/references/"+file+" has no anchor #"+slug)
				}
			}
		}
	}
	for _, name := range mustPoint {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			errs = append(errs, name+": "+err.Error())
		} else if !strings.Contains(string(data), "crw-canon") {
			errs = append(errs, name+": does not name crw-canon, where the shared sections live")
		}
	}
	return errs
}

// namedConsumers are the consumers outside the skills that the shared sections' move changes.
var namedConsumers = []string{
	"AGENTS.md",
	"CONTRIBUTING.md",
	"README.md",
	"docs/plugin-packaging.md",
	"docs/relay/dag-scheduler.md",
	"internal/dev/ci/delivery_authority_wording_test.go",
}

func TestSharedSectionConsumersMoved(t *testing.T) {
	t.Run("the checked-in tree", func(t *testing.T) {
		if errs := consumerErrors(repoRoot(), namedConsumers); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	t.Run("fixture: an old anchor, a dead canon anchor and the old design path fail", func(t *testing.T) {
		r := newRepo(t)
		r.write(canonRefDir+"release-gate.md", "# Release gate\n\n## The release gate\n")
		r.write(planIntegrate, "# T\n\n[a](#the-release-gate)\n")
		r.write("docs/a.md", "[x](../plugins/crw/skills/crw-plan/references/integrations.md#the-release-gate)\n[y](../plugins/crw/skills/crw-canon/references/release-gate.md#nowhere)\n[z](../plugins/crw/skills/crw-plan/references/design-explanation.md)\n[ok](../plugins/crw/skills/crw-canon/references/release-gate.md#the-release-gate)\n")
		r.write("AGENTS.md", "plain\n")
		r.commit()
		joined := strings.Join(consumerErrors(r.root, []string{"AGENTS.md"}), "\n")
		for _, want := range []string{
			planIntegrate + ":3: links to (#the-release-gate)",
			"docs/a.md:1: points at integrations.md#the-release-gate",
			"docs/a.md:2: crw-canon/references/release-gate.md has no anchor #nowhere",
			"docs/a.md:3: design-explanation.md moved",
			"AGENTS.md: does not name crw-canon",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("errors lack %q:\n%s", want, joined)
			}
		}
		if strings.Contains(joined, "docs/a.md:4") {
			t.Errorf("a live canon link was reported:\n%s", joined)
		}
	})
}

// tableRows is the data rows of the first Markdown table in text whose header's first cell is first.
func tableRows(text, first string) [][]string {
	var rows [][]string
	in := false
	for _, line := range lines(text) {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			if in {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if !in {
			if cells[0] == first {
				in = true
			}
			continue
		}
		if strings.Trim(cells[0], "-: ") == "" {
			continue
		}
		rows = append(rows, cells)
	}
	return rows
}

var (
	inlineCode  = regexp.MustCompile("`[^`]*`")
	relayRoleID = regexp.MustCompile("`(supervisor|parent|child)`")
	junTalks    = regexp.MustCompile(`(?i)top session[^.\n]*\btalks? with Jun|\bJun talks with the top session`)
)

// TestHierarchyTableRoleIds: the one level table of hierarchy.md (CRW-1352, design-1316 section 18.1) names the four layers
// in order with the relay role ids the store records: none for the top session and the subagents, `supervisor` for the parent
// and `parent` for the child. The ids are not renamed, and the table says that the relay's `parent` is the child.
func TestHierarchyTableRoleIds(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), canonRefDir, "hierarchy.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	rows := tableRows(text, "Korean")
	want := []struct{ korean, english, relay string }{
		{"총괄", "top session", ""},
		{"부모", "parent (repository session)", "`supervisor`"},
		{"자식", "child (project session)", "`parent`"},
		{"서브에이전트", "subagents (architect, executor, reviewer, explorer)", ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("level table has %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for i, w := range want {
		row := rows[i]
		if len(row) < 5 {
			t.Fatalf("row %d has %d cells: %v", i+1, len(row), row)
		}
		if row[0] != w.korean || row[1] != w.english {
			t.Errorf("row %d = %q / %q, want %q / %q", i+1, row[0], row[1], w.korean, w.english)
		}
		if w.relay == "" {
			if relayRoleID.MatchString(row[2]) {
				t.Errorf("%s row names a relay role id: %q", w.english, row[2])
			}
		} else if !strings.HasPrefix(row[2], w.relay) {
			t.Errorf("%s relay role id = %q, want %q", w.english, row[2], w.relay)
		}
	}
	if !junTalks.MatchString(normalized(text)) {
		t.Errorf("hierarchy.md does not say that the top session talks with Jun")
	}
	if !strings.Contains(normalized(text), "The relay role id `parent` names the child, not the parent") {
		t.Errorf("hierarchy.md does not warn that the relay's `parent` is the child")
	}
	if n := utf8.RuneCountInString(text); n < 2000 {
		t.Errorf("hierarchy.md is only %d characters", n)
	}
}

// modelNames is what execution-roles.md must not name: a model, a model family or an effort word.
var modelNames = regexp.MustCompile(`(?i)haiku|sonnet|opus|fable|gpt-?[0-9]|\bSOL\b|\bluna\b|gemini|\bglm|kimi|qwen|xhigh`)

func TestExecutionRolesNamesRoleKeysOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), canonRefDir, "execution-roles.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if found := modelNames.FindAllString(text, -1); len(found) > 0 {
		t.Errorf("execution-roles.md names models or efforts: %v", found)
	}
	for _, key := range []string{"supervisor", "parent", "child", "architect", "executor", "explorer", "reviewer"} {
		if !strings.Contains(text, "`"+key+"`") {
			t.Errorf("execution-roles.md does not name the policy key `%s` in code font", key)
		}
	}
	if rows := tableRows(text, "Policy key"); len(rows) < 7 {
		t.Errorf("execution-roles.md key table has %d rows, want the 3 relay and 4 agent roles", len(rows))
	}
	t.Run("fixture: the model pattern", func(t *testing.T) {
		if !modelNames.MatchString("the child runs on Sonnet") || !modelNames.MatchString("gpt-6-1") || modelNames.MatchString("the `child` policy key") {
			t.Errorf("model name pattern is wrong")
		}
	})
}

func TestCanonSkillNeverImplicit(t *testing.T) {
	root := repoRoot()
	yaml, err := os.ReadFile(filepath.Join(root, canonSkillDir, "agents/openai.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	policy := regexp.MustCompile(`(?m)^policy:\n  allow_implicit_invocation: false$`)
	if !policy.Match(yaml) || strings.Contains(string(yaml), "allow_implicit_invocation: true") {
		t.Errorf("crw-canon/agents/openai.yaml does not set policy.allow_implicit_invocation: false:\n%s", yaml)
	}
	skill, err := os.ReadFile(filepath.Join(root, canonSkillDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(skill), "---\nname: crw-canon\n") {
		t.Errorf("crw-canon/SKILL.md does not start with the frontmatter name crw-canon")
	}
	descriptions, err := skillDescriptions(root)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(descriptions["crw-canon"]); n == 0 || n > maxDescriptionRunes {
		t.Errorf("crw-canon description has %d characters, want 1-%d", n, maxDescriptionRunes)
	}
	// The index links every reference file, and the skill holds nothing else in its root.
	entries, err := os.ReadDir(filepath.Join(root, canonRefDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.Contains(string(skill), "(references/"+e.Name()+")") {
			t.Errorf("crw-canon/SKILL.md does not link references/%s", e.Name())
		}
	}
	for _, s := range canonMoved {
		if !strings.Contains(string(skill), "(references/"+s.File+")") {
			t.Errorf("crw-canon/SKILL.md does not link references/%s", s.File)
		}
	}
	top, err := os.ReadDir(filepath.Join(root, canonSkillDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range top {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"SKILL.md", "agents", "references"}) {
		t.Errorf("crw-canon holds %v, want SKILL.md, agents and references", names)
	}
	// It joins the skills CRW wrote itself, so the skillport check does not look for a record.
	record, err := os.ReadFile(filepath.Join(root, "internal/dev/skillport/record.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(record), `"crw-canon"`) {
		t.Errorf("crw-canon is not in skillport.OwnSkills")
	}
}

// koreanLevelWord is a Korean word for a level: the level table's Korean column and the older
// session words for the same levels.
var koreanLevelWord = regexp.MustCompile(`감독|부모|자식|관리`)

// The Korean words for the levels are the Korean column of the one level table in hierarchy.md. No
// other file of the shared library names the levels in Korean, so a Korean report or record cannot
// read one level under the word another file gives it; the delivery rule for Korean reports points at
// that table instead.
func TestKoreanLevelWordsOnlyInHierarchyTable(t *testing.T) {
	root := repoRoot()
	var found []string
	err := filepath.WalkDir(filepath.Join(root, canonSkillDir), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == canonRefDir+"hierarchy.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range lines(string(data)) {
			if koreanLevelWord.MatchString(line) {
				found = append(found, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("Korean level words outside the hierarchy.md level table:\n%s", strings.Join(found, "\n"))
	}
	delivery := readRepoText(t, canonRefDir+"delivery.md")
	if !strings.Contains(delivery, "(hierarchy.md#the-levels)") {
		t.Errorf("delivery.md does not point Korean reports at the level table (hierarchy.md#the-levels)")
	}
}

// The two kinds of key in execution-roles.md are held by two different implementations: the host's
// execution policy (internal/bridge/execution) accepts the relay roles and refuses any other key, and
// the helper role store (internal/role) holds the agent roles, the four helper roles and the two
// flow-only keys (CRW-1330). The document's Kind column and its get_capabilities rule have to follow
// that split, not a table of nine keys.
func TestExecutionRolesKindsMatchTheirPolicies(t *testing.T) {
	text := readRepoText(t, canonRefDir+"execution-roles.md")
	kinds := map[string]string{}
	for _, row := range tableRows(text, "Policy key") {
		if len(row) < 2 {
			t.Fatalf("key table row %v has no Kind cell", row)
		}
		kinds[strings.Trim(row[0], "`")] = row[1]
	}
	var helperKeys []string
	for _, r := range append(role.Roles(), role.FlowOnlyRoles()...) {
		helperKeys = append(helperKeys, string(r))
	}
	relayKeys := []string{execution.Supervisor, execution.Parent, execution.Child}
	if len(kinds) != len(relayKeys)+len(helperKeys) {
		t.Errorf("the key table names %d keys, the two policies hold %d relay and %d helper roles", len(kinds), len(relayKeys), len(helperKeys))
	}
	acceptsKey := func(key string) error {
		body := `{"model":"m","reasoningEffort":"e"}`
		if key == execution.Supervisor {
			body = `{}`
		}
		_, err := execution.FromBytes([]byte(fmt.Sprintf(`{"roles":{%s:%s}}`, mustJSON(key), body)), "policy")
		return err
	}
	for _, key := range relayKeys {
		if err := acceptsKey(key); err != nil {
			t.Errorf("the host execution policy refuses the relay role %q: %v", key, err)
		}
		if kinds[key] != "relay role" {
			t.Errorf("%q is a role of the host execution policy; its Kind is %q, want \"relay role\"", key, kinds[key])
		}
	}
	for _, key := range helperKeys {
		if err := acceptsKey(key); err == nil {
			t.Errorf("the host execution policy accepts %q, which the table lists as a helper role", key)
		}
		if kinds[key] != "agent role" {
			t.Errorf("%q is a role of the helper store; its Kind is %q, want \"agent role\"", key, kinds[key])
		}
	}
	t.Run("the get_capabilities rule is about relay roles only", func(t *testing.T) {
		for _, para := range strings.Split(text, "\n\n") {
			if !strings.Contains(para, "get_capabilities") && !strings.Contains(para, "host's policy does not declare") {
				continue
			}
			if !strings.Contains(para, "relay role") {
				t.Errorf("a paragraph that sends a reader to get_capabilities or the host policy does not say it is about the relay roles:\n%s", para)
			}
			for _, key := range helperKeys {
				if strings.Contains(para, "`"+key+"`") {
					t.Errorf("a paragraph that sends a reader to get_capabilities or the host policy names the helper role `%s`:\n%s", key, para)
				}
			}
		}
	})
	t.Run("helper settings are read and recovered through crw role helper", func(t *testing.T) {
		if !strings.Contains(normalized(text), "`crw role helper`") {
			t.Errorf("execution-roles.md does not send helper role settings to `crw role helper`")
		}
	})
	t.Run("each helper row names the helper store as its settings source and the operator as its decider", func(t *testing.T) {
		if got := helperRowProblems(text, helperKeys); len(got) > 0 {
			t.Errorf("execution-roles.md helper rows: %s", strings.Join(got, "\n  "))
		}
	})
	t.Run("a table that reverts only the settings source and decider of the helper rows is refused", func(t *testing.T) {
		reverted := text
		for _, key := range helperKeys {
			for _, row := range tableRows(reverted, "Policy key") {
				if strings.Trim(row[0], "`") != key {
					continue
				}
				if len(row) < 5 {
					t.Fatalf("key table row %v lacks the settings and decider cells", row)
				}
				old := "| " + strings.Join(row, " | ") + " |"
				wrong := "| " + strings.Join(row[:3], " | ") + " | the policy's `" + key + "` entry | the policy |"
				if !strings.Contains(reverted, old) {
					t.Fatalf("row %q is not found verbatim in the table", old)
				}
				reverted = strings.Replace(reverted, old, wrong, 1)
			}
		}
		if reverted == text {
			t.Fatal("the fixture changed nothing")
		}
		for _, row := range tableRows(reverted, "Policy key") {
			for _, key := range helperKeys {
				if strings.Trim(row[0], "`") == key && row[1] != "agent role" {
					t.Fatalf("the fixture changed the Kind of %q", key)
				}
			}
		}
		if got := helperRowProblems(reverted, helperKeys); len(got) != 2*len(helperKeys) {
			t.Errorf("the reverted table reports %d problems, want a settings and a decider problem for each of %d helper rows: %v", len(got), len(helperKeys), got)
		}
	})
}

// A helper role is never read from the host execution policy: its settings are the helper role
// store's (internal/role Roles, ResolveSpawnConfig), read and reset with crw role helper, and the
// operator decides them through that command.
func helperRowProblems(text string, helperKeys []string) []string {
	rows := map[string][]string{}
	for _, row := range tableRows(text, "Policy key") {
		rows[strings.Trim(row[0], "`")] = row
	}
	var problems []string
	for _, key := range helperKeys {
		row, ok := rows[key]
		if !ok || len(row) < 5 {
			problems = append(problems, key+": no row with settings and decider cells")
			continue
		}
		if !strings.Contains(row[3], "`crw role helper`") || !strings.Contains(strings.ToLower(row[3]), "helper role store") {
			problems = append(problems, key+": settings come from "+row[3]+", not the helper role store read with `crw role helper`")
		}
		if !strings.Contains(row[4], "`crw role helper`") || !strings.Contains(strings.ToLower(row[4]), "operator") {
			problems = append(problems, key+": decided by "+row[4]+", not the operator through `crw role helper`")
		}
	}
	return problems
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// The level nouns are what an instruction uses; the Korean words of the level table in hierarchy.md are
// shown to a reader there and nowhere else. Current skill metadata, the references and the README name a
// level by its English noun (parent for the repository session, child for the project session), so a Korean
// level word outside the table is a second definition of a level.
func TestCurrentSurfacesUseNoLevelWordOfTheOldMapping(t *testing.T) {
	root := repoRoot()
	var found []string
	skills, err := filepath.Glob(filepath.Join(root, "plugins/crw/skills/*"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, dir := range skills {
		files = append(files, filepath.Join(dir, "SKILL.md"), filepath.Join(dir, "agents/openai.yaml"))
		refs, _ := filepath.Glob(filepath.Join(dir, "references/*.md"))
		files = append(files, refs...)
	}
	files = append(files, filepath.Join(root, "README.md"))
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		if filepath.ToSlash(rel) == canonRefDir+"hierarchy.md" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range lines(string(data)) {
			// `자식 pair` is the label an issue body already carries for the child pair line, a literal and not a level.
			if regexp.MustCompile(`부모|자식`).MatchString(strings.ReplaceAll(line, "자식 pair", "")) {
				found = append(found, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(found) > 0 {
		t.Errorf("Korean level words of the level table outside it (name the level by its noun):\n%s", strings.Join(found, "\n"))
	}
}

// A historical record cites the file and the line range it read at its own revision. A line range
// that the file it now names cannot have, because the file is shorter, is a citation that was
// rewritten without being re-read.
var rangeCitation = regexp.MustCompile("`([A-Za-z0-9_./-]*(?:hierarchy|delivery|linear-model|release-gate|schedule-contract|execution-roles|design-explanation|skill-family)\\.md)`\\s*\\(?(\\d+)(?: to (\\d+))?")

func TestCitedLineRangesExistInTheCitedCanonFile(t *testing.T) {
	root := repoRoot()
	var bad []string
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range rangeCitation.FindAllStringSubmatch(string(data), -1) {
			target, err := os.ReadFile(filepath.Join(root, canonRefDir, filepath.Base(m[1])))
			if err != nil {
				continue
			}
			count := len(lines(string(target)))
			for _, n := range m[2:] {
				if v, _ := strconv.Atoi(n); v > count {
					rel, _ := filepath.Rel(root, path)
					bad = append(bad, fmt.Sprintf("%s cites %s at line %s; the file has %d lines", filepath.ToSlash(rel), m[1], n, count))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("cited line numbers that the file cannot have:\n%s", strings.Join(bad, "\n"))
	}
}
