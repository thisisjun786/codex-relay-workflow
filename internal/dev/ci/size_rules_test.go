//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The size rules are procedure sentences of the skills: crw-define sizes an initiative, crw-plan sizes
// projects, milestones and issues, and crw-add-issue tells its caller when a limit is passed. Each
// test reads the skill text that carries the rule and holds it to the numbers the code fixes.

// sectionOf is the body of the first heading with the given text, up to the next heading of the same
// or a higher level, outside fenced code. It is empty when the heading is absent.
func sectionOf(text, heading string) string {
	var out []string
	level := 0
	fence := ""
	for _, line := range lines(text) {
		if marker := fenceMarker(strings.TrimLeft(line, " ")); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case marker[0] == fence[0] && len(marker) >= len(fence):
				fence = ""
			}
		} else if fence == "" {
			if m := atxHeadingLine.FindStringSubmatch(line); m != nil {
				body := strings.TrimLeft(line, " ")
				n := len(body) - len(strings.TrimLeft(body, "#"))
				switch {
				case level == 0 && m[1] == heading:
					level = n
					continue
				case level != 0 && n <= level:
					return strings.Join(out, "\n")
				}
			}
		}
		if level != 0 {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// normalized is text with every run of whitespace reduced to one space.
func normalized(text string) string { return strings.Join(strings.Fields(text), " ") }

func readRepoText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// wordingMissing is the patterns the text does not match.
func wordingMissing(text string, patterns []string) []string {
	var missing []string
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(text) {
			missing = append(missing, p)
		}
	}
	return missing
}

// dagLimit is a limit constant of internal/relay/dag/model.go.
func dagLimit(t *testing.T, name string) int {
	t.Helper()
	data := readRepoText(t, "internal/relay/dag/model.go")
	m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*(\d+)`).FindStringSubmatch(data)
	if m == nil {
		t.Fatalf("internal/relay/dag/model.go has no %s", name)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// laterSizeFields are the fields and the proposer that the planning documents carry (crw-plan, crw-add-issue) since the second skill
// series (CRW-1222); the initiative's size rules (crw-define) state none of them.
var laterSizeFields = regexp.MustCompile(`size_fit|size_reason|size-fit|\bgroup:`)

func TestPlanSizeRulesWording(t *testing.T) {
	if got := dagLimit(t, "MaxNodes"); got != 64 {
		t.Fatalf("the plan limit MaxNodes is %d; the size rules say 64", got)
	}
	plan := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-plan/SKILL.md"), "Size rules"))
	if plan == "" {
		t.Fatal("crw-plan/SKILL.md has no Size rules section")
	}
	t.Run("crw-plan sizes projects, milestones and issues", func(t *testing.T) {
		missing := wordingMissing(plan, []string{
			`(?i)integration target`,
			fmt.Sprintf(`at most %d (live )?issues`, dag.MaxNodes),
			"`MaxNodes`",
			`4-20 (is )?recommended`,
			`(?i)edit-region map[^.]*named artifacts`,
			`(?i)milestone[^.]*one observable result`,
			`at most 8 issues recommended`,
			`over 12 issues[^.]*split`,
			`(?i)issue is a concept bundle`,
			`(?i)wish to parallelize never splits an issue`,
			"`crw skill issue-size check`",
			"`ok`", "`over_line`", "`over_line_accepted`",
			`(?i)exception record`,
			`(?i)concept questions`,
			`(?i)never by the count alone|no count alone`,
			`(?i)no model name and no (reasoning )?effort`,
		})
		if len(missing) > 0 {
			t.Errorf("crw-plan Size rules lack: %s\n%s", strings.Join(missing, "\n  "), plan)
		}
	})
	t.Run("the section carries no model name", func(t *testing.T) {
		stripped := strings.ReplaceAll(strings.ToLower(plan), "no model name and no effort", "")
		if found := modelNames.FindString(stripped); found != "" {
			t.Errorf("Size rules names %q", found)
		}
	})
	t.Run("crw-define sizes an initiative", func(t *testing.T) {
		define := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-define/SKILL.md"), "Size rules"))
		if define == "" {
			t.Fatal("crw-define/SKILL.md has no Size rules section")
		}
		missing := wordingMissing(define, []string{
			`(?i)initiative is one outcome`,
			`(?i)list[^.]*projects`,
			`(?i)artifacts? (that )?one project hands to another|hands? to another`,
			`(?i)run in parallel`,
			`(?i)order`,
			`(?i)observable finish condition (for|of) each project`,
			`at most 8 projects (in flight|running at the same time)`,
			`(?i)recommended`,
		})
		if len(missing) > 0 {
			t.Errorf("crw-define Size rules lack: %s\n%s", strings.Join(missing, "\n  "), define)
		}
		if found := laterSizeFields.FindString(define); found != "" {
			t.Errorf("Size rules names %q, which an initiative does not carry", found)
		}
	})
}

// addIssueCallers are the callers crw-add-issue names.
var addIssueCallers = []string{`project orchestrator`, `initiative lead`, `user`}

func TestAddIssueCallersNamed(t *testing.T) {
	text := readRepoText(t, "plugins/crw/skills/crw-add-issue/SKILL.md")
	callers := normalized(sectionOf(text, "Who calls"))
	if callers == "" {
		t.Fatal("crw-add-issue/SKILL.md has no Who calls section")
	}
	for _, caller := range addIssueCallers {
		if !strings.Contains(callers, caller) {
			t.Errorf("Who calls does not name the %s", caller)
		}
	}
	missing := wordingMissing(callers, []string{
		`(?i)executor[^.]*report[^.]*defect[^.]*receipt`,
		`(?i)region map entry`,
	})
	if len(missing) > 0 {
		t.Errorf("Who calls lacks: %s\n%s", strings.Join(missing, "\n  "), callers)
	}
	t.Run("the description is one job in at most 300 characters", func(t *testing.T) {
		descriptions, err := skillDescriptions(repoRoot())
		if err != nil {
			t.Fatal(err)
		}
		if n := utf8.RuneCountInString(descriptions["crw-add-issue"]); n == 0 || n > maxDescriptionRunes {
			t.Errorf("crw-add-issue description has %d characters, want 1-%d", n, maxDescriptionRunes)
		}
	})
}

func TestAddIssueAdvisoryLimitsWording(t *testing.T) {
	text := readRepoText(t, "plugins/crw/skills/crw-add-issue/SKILL.md")
	if got := dagLimit(t, "MaxNodes"); got != 64 {
		t.Fatalf("the plan limit MaxNodes is %d; crw-add-issue says 64", got)
	}
	advisory := normalized(sectionOf(text, "Tell the caller about the limits"))
	if advisory == "" {
		t.Fatal("crw-add-issue/SKILL.md has no section Tell the caller about the limits")
	}
	missing := wordingMissing(advisory, []string{
		`(?i)advisory`,
		fmt.Sprintf(`(?i)(project|plan)[^.;]*more than %d issues`, dag.MaxNodes),
		`(?i)milestone[^.;]*more than 12 issues`,
		`(?i)sentence of this procedure[^.]*not a warning (of|from) an executable`,
	})
	if len(missing) > 0 {
		t.Errorf("the advisory section lacks: %s\n%s", strings.Join(missing, "\n  "), advisory)
	}
	// The procedure ends with the readiness check.
	last := normalized(sectionOf(text, "Check, write, read back"))
	if !strings.Contains(last, "`crw skill issue-ready check`") {
		t.Errorf("the closing section does not run `crw skill issue-ready check`")
	}
}

// An initiative lead calls crw-add-issue but writes no project record: the project orchestrator owns
// the project and its issues, so the initiative lead hands it the drafted body and the orchestrator
// registers it.
func TestAddIssueInitiativeLeadHandsTheBodyToTheOwner(t *testing.T) {
	text := readRepoText(t, "plugins/crw/skills/crw-add-issue/SKILL.md")
	callers := normalized(sectionOf(text, "Who calls"))
	if regexp.MustCompile(`(?i)initiative lead (registers|creates|writes) an issue`).MatchString(callers) {
		t.Errorf("Who calls lets an initiative lead register an issue itself:\n%s", callers)
	}
	missing := wordingMissing(callers, []string{
		`(?i)initiative lead[^.]*writes nothing`,
		`(?i)initiative lead[^.]*hands[^.]*body[^.]*project orchestrator`,
		`(?i)project orchestrator[^.]*registers it`,
		`\(\.\./crw-canon/references/hierarchy\.md#scope-of-each-level\)`,
	})
	if len(missing) > 0 {
		t.Errorf("Who calls lacks: %s\n%s", strings.Join(missing, "\n  "), callers)
	}
	write := normalized(sectionOf(text, "Check, write, read back"))
	if !regexp.MustCompile(`(?i)write intent[^.]*(from )?(a|the) project orchestrator or (a|the) user`).MatchString(write) {
		t.Errorf("Check, write, read back does not limit the write to a project orchestrator or the user:\n%s", write)
	}
}

// planNodes is the revision that adds the nodes n000.. to a plan.
func planNodes(from, to int) []dag.Change {
	var changes []dag.Change
	for i := from; i < to; i++ {
		id := fmt.Sprintf("n%03d", i)
		changes = append(changes, dag.Change{Op: dag.OpAddNode, Node: &dag.Node{NodeID: id, IssueKey: "CRW-" + id, Kind: dag.NodeNonPR, CriteriaSetDigest: testsupport.Dig("criteria " + id)}})
	}
	return changes
}

// MaxNodes bounds the live nodes of one plan revision, not the issues a Linear project ever held:
// a retired node leaves the count. The size rules say what the bound counts, because a project of
// sixty finished issues and five open ones is not over it.
func TestPlanLimitIsTheLiveNodesOfOneRevision(t *testing.T) {
	first := dag.Revision{PlanID: "p", ProjectKey: "k", Changes: planNodes(0, dag.MaxNodes)}
	state, _, violations := dag.Apply(dag.State{}, first)
	if len(violations) > 0 {
		t.Fatalf("%d live nodes: %v", dag.MaxNodes, violations)
	}
	replaced := dag.Revision{PlanID: "p", ProjectKey: "k", ExpectedParent: state.Revision,
		Changes: append([]dag.Change{{Op: dag.OpRetireNode, NodeID: "n000"}}, planNodes(dag.MaxNodes, dag.MaxNodes+1)...)}
	if _, _, violations := dag.Apply(state, replaced); len(violations) > 0 {
		t.Errorf("retiring one node and adding one is refused: %v", violations)
	}
	over := dag.Revision{PlanID: "p", ProjectKey: "k", ExpectedParent: state.Revision, Changes: planNodes(dag.MaxNodes, dag.MaxNodes+1)}
	if _, _, violations := dag.Apply(state, over); len(violations) == 0 || violations[0].Rule != dag.RuleLimitExceeded {
		t.Errorf("a 65th live node is not refused with %s: %v", dag.RuleLimitExceeded, violations)
	}
	live := regexp.MustCompile(`(?i)\blive\b`)
	retired := regexp.MustCompile(`(?i)retired`)
	plan := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-plan/SKILL.md"), "Size rules"))
	var bullet string
	for _, b := range strings.Split(plan, "- ") {
		if strings.Contains(b, "`MaxNodes`") {
			bullet = b
		}
	}
	if bullet == "" || !live.MatchString(bullet) || !retired.MatchString(bullet) {
		t.Errorf("the project size rule does not say that MaxNodes bounds the live nodes of one plan revision, a retired node leaving the count:\n%s", bullet)
	}
	advisory := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-add-issue/SKILL.md"), "Tell the caller about the limits"))
	if !live.MatchString(advisory) || !retired.MatchString(advisory) {
		t.Errorf("the add-issue advisory counts every issue of the project, not the live nodes of its plan:\n%s", advisory)
	}
}

// The decided acceptance rule is one integration target per project. Work in several repositories is
// one project per repository, an initiative holding the projects; an issue never carries a target of
// its own. targetRuleProblems judges one text and is itself tested with accepted and rejected texts.
var targetRuleForbidden = []struct{ pattern, why string }{
	{`(?i)(project|projects) (may|can) (span|involve|own)[^.]*repositor`, "a project spanning repositories"},
	{`(?i)repositories can belong to one project`, "several repositories in one project"},
	{`(?i)one project may involve several repositories`, "several repositories in one project"},
	{`(?i)(keep|keeps) a multi-repository\s+outcome in one project`, "a multi-repository outcome kept in one project"},
	{`(?i)target is the issue's`, "an integration target belonging to the issue"},
	{`(?i)issues? (is|are) delivered into (its own|one) integration target`, "an integration target belonging to the issue"},
	{`(?i)(two|several|more than one|many) integration targets`, "a project with more than one integration target"},
	{`(?i)\bproject\b[^.]*\bissues each carry their own[^.]*repository label`, "issues of one project in separate repositories"},
	{`(?i)repository labels? each\b[^|]*\bmilestone`, "issues of separate repositories gathered in one milestone of one project"},
}

func targetRuleProblems(text string) []string {
	text = normalized(text)
	var problems []string
	for _, f := range targetRuleForbidden {
		if regexp.MustCompile(f.pattern).MatchString(text) {
			problems = append(problems, f.why)
		}
	}
	return problems
}

func TestTargetRuleJudgesOneTargetPerProject(t *testing.T) {
	accepted := []string{
		"A project has one integration target and at most 64 live issues.",
		"Work in several repositories is one project per repository; an initiative holds the projects, each with its own single integration target.",
		"For example, a requested “Complete installation and first launch” initiative holds one project for core, one for desktop and one for installer, each project with its own single integration target and its issues carrying that repository label.",
		"| 2 projects, one per repository, held by 1 initiative, 1 issue and 1 repository label each, installer blocked by core, combined outcome on the initiative. | Different target repository. |",
	}
	for _, text := range accepted {
		if got := targetRuleProblems(text); len(got) > 0 {
			t.Errorf("the per-project rule is rejected (%v): %s", got, text)
		}
	}
	rejected := []string{
		"A project has two integration targets.",
		"A project may span repositories, so the target is the issue's and not the project's.",
		"Multiple repositories can belong to one project.",
		"Each of its issues is delivered into one integration target.",
		// The examples round 4 found in the canon and the plan boundaries, as they stood.
		"For example, a requested “Complete installation and first launch” project can have one product family while its core, desktop, and installer issues each carry their own edit-target repository label.",
		"| 1 | An outcome needs a change in the core repository and in the installer repository; the installer consumes the new core interface. | 2 issues, 1 repository label each, installer blocked by core, combined outcome on the milestone, 0 extra issues for reading core. | Different target repository. |",
	}
	for _, text := range rejected {
		if got := targetRuleProblems(text); len(got) == 0 {
			t.Errorf("a project with more than one target is accepted: %s", text)
		}
	}
}

func TestPlanSizeRuleKeepsOneIntegrationTargetPerProject(t *testing.T) {
	plan := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-plan/SKILL.md"), "Size rules"))
	var bullet string
	for _, b := range strings.Split(plan, "- ") {
		if strings.HasPrefix(b, "A project") {
			bullet = b
		}
	}
	if !regexp.MustCompile(`(?i)one integration target`).MatchString(bullet) {
		t.Errorf("the project size rule does not give a project one integration target:\n%s", bullet)
	}
	if !regexp.MustCompile(`(?i)one project per repository`).MatchString(bullet) {
		t.Errorf("the project size rule does not say that work in several repositories is one project per repository:\n%s", bullet)
	}
	for _, path := range []string{
		"plugins/crw/skills/crw-plan/SKILL.md",
		"plugins/crw/skills/crw-plan/references/integrations.md",
		"plugins/crw/skills/crw-plan/references/issue-boundaries.md",
		canonRefDir + "linear-model.md",
		canonRefDir + "hierarchy.md",
	} {
		if got := targetRuleProblems(readRepoText(t, path)); len(got) > 0 {
			t.Errorf("%s contradicts the one integration target per project rule: %v", path, got)
		}
	}
}
