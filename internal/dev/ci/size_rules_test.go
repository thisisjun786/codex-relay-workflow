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

// laterSizeFields are the fields and the proposer that later issues bring into the documents; the size
// sentences of this change state today's facts only.
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
	t.Run("the section carries no model name and no later field", func(t *testing.T) {
		stripped := strings.ReplaceAll(strings.ToLower(plan), "no model name and no effort", "")
		if found := modelNames.FindString(stripped); found != "" {
			t.Errorf("Size rules names %q", found)
		}
		if found := laterSizeFields.FindString(plan); found != "" {
			t.Errorf("Size rules names %q, which a later issue brings in", found)
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
			t.Errorf("Size rules names %q, which a later issue brings in", found)
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
	if found := laterSizeFields.FindString(text); found != "" {
		t.Errorf("crw-add-issue names %q, which a later issue brings in", found)
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

// A project may span repositories (one issue per repository delivery, linear-model.md), so the
// integration target of the size rule is the target of each issue's delivery, not one per project.
func TestPlanSizeRuleKeepsMultiRepositoryProjects(t *testing.T) {
	plan := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-plan/SKILL.md"), "Size rules"))
	var bullet string
	for _, b := range strings.Split(plan, "- ") {
		if strings.HasPrefix(b, "A project") {
			bullet = b
		}
	}
	if !regexp.MustCompile(`(?i)repositor`).MatchString(bullet) {
		t.Errorf("the project size rule does not say how it reads for a project spanning repositories:\n%s", bullet)
	}
	if regexp.MustCompile(`(?i)one integration target and at most`).MatchString(bullet) {
		t.Errorf("the project size rule requires one integration target per project, which the multi-repository rule of the same skill permits:\n%s", bullet)
	}
}
