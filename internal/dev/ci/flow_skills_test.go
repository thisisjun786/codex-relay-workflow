//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The skills that describe the flow engine (CRW-1222, the second skill series): the page of the commands (crw-canon/references/flow.md),
// the project orchestrator's procedure for a flow run (crw-run), the initiative lead's reading of milestone reports, the size fields of
// the plan, and the status reader. Each check reads the text a reader follows and holds it to the facts of the code or to the order the
// procedure needs; a judge is itself run on a text that lacks the sentence.

const flowRef = canonRefDir + "flow.md"

var numberedStep = regexp.MustCompile(`^\s*[0-9]+\. `)

// ownershipSection is the section of flow.md that says who owns control in a flow-managed run.
func ownershipSection(text string) string {
	return sectionOf(text, "Who owns control in a flow-managed run")
}

// flowOwnershipProblems judges the control ownership section of flow.md.
func flowOwnershipProblems(section string) []string {
	var problems []string
	flat := normalized(section)
	if flat == "" {
		return []string{"flow.md has no section \"Who owns control in a flow-managed run\""}
	}
	// The table: one row for each thing the run owns, and the run is named in the owner cell.
	rows := tableRows(section, "Control")
	owners := map[string]string{}
	for _, row := range rows {
		if len(row) >= 2 {
			owners[strings.ToLower(row[0])] = strings.ToLower(row[1])
		}
	}
	for _, concern := range []string{"phases", "audit", "evidence"} {
		found := false
		for key, owner := range owners {
			if strings.Contains(key, concern) && strings.HasPrefix(owner, "the run") {
				found = true
			}
		}
		if !found {
			problems = append(problems, "the table has no row for "+concern+" whose owner is the run")
		}
	}
	for _, p := range wordingMissing(flat, []string{
		`(?i)the executor and an oversized (issue )?child start no PABCD state machine`,
		`(?i)the standalone PABCD rules are unchanged`,
		`(?i)reviewer-final does not replace the pre-build design audit`,
		`(?i)another model family than the architect`,
		`(?i)before any executor starts`,
		`(?i)needs_changes[^.]*returns to the architect`,
		`(?i)replan[^.]*audited again`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	// The order of the C4 procedure is the order of its numbered steps: DESIGN, then the audit, then the candidate review of each issue,
	// then the final gate with reviewer-final.
	var steps []string
	for _, line := range lines(section) {
		if numberedStep.MatchString(line) {
			steps = append(steps, normalized(line))
		}
	}
	wantSteps := [][]string{{"architect DESIGN"}, {"pre-build design audit"}, {"candidate review"}, {"final gate", "reviewer-final"}}
	if len(steps) != len(wantSteps) {
		problems = append(problems, "the high-risk procedure has "+strconv.Itoa(len(steps))+" numbered steps, want 4")
	} else {
		for i, words := range wantSteps {
			for _, word := range words {
				if !strings.Contains(steps[i], word) {
					problems = append(problems, "step "+strconv.Itoa(i+1)+" of the high-risk procedure does not name \""+word+"\"")
				}
			}
		}
	}
	return problems
}

func TestFlowControlOwnershipTableWording(t *testing.T) {
	section := ownershipSection(readRepoText(t, flowRef))
	if problems := flowOwnershipProblems(section); len(problems) > 0 {
		t.Errorf("%s: %s\n%s", flowRef, strings.Join(problems, "\n  "), normalized(section))
	}
	// The skill that classifies the work carries the same order for a C4 issue, so the two readers agree.
	dev := normalized(readRepoText(t, "plugins/crw/skills/crw-dev/SKILL.md"))
	for _, want := range []string{"independent pre-build design audit by a reviewer of a different model family before any executor starts", "reviewer-final does not replace the pre-build design audit"} {
		if !strings.Contains(dev, want) {
			t.Errorf("crw-dev/SKILL.md lacks %q", want)
		}
	}

	t.Run("fixture: each missing sentence is reported", func(t *testing.T) {
		good := "## Who owns control in a flow-managed run\n\n| Control | Owner |\n|---|---|\n| Phases | the run |\n| Audit | the run |\n| Evidence | the run |\n\n" +
			"The executor and an oversized child start no PABCD state machine. The standalone PABCD rules are unchanged.\n\n" +
			"1. The architect DESIGN.\n2. The pre-build design audit, by a reviewer of another model family than the architect, before any executor starts; needs_changes returns to the architect and a replan is audited again.\n" +
			"3. The candidate review of each issue.\n4. After integration the final gate and reviewer-final. Reviewer-final does not replace the pre-build design audit.\n"
		if p := flowOwnershipProblems(sectionOf(good, "Who owns control in a flow-managed run")); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(string) string{
			"no section": func(s string) string {
				return strings.Replace(s, "## Who owns control in a flow-managed run", "## Other", 1)
			},
			"audit unowned": func(s string) string { return strings.Replace(s, "| Audit | the run |", "| Audit | the executor |", 1) },
			"fsm allowed": func(s string) string {
				return strings.Replace(s, "start no PABCD state machine", "start the PABCD state machine", 1)
			},
			"final replaces": func(s string) string {
				return strings.Replace(s, "Reviewer-final does not replace", "Reviewer-final replaces", 1)
			},
			"audit after run": func(s string) string {
				return strings.Replace(s, "before any executor starts", "after the executors", 1)
			},
			"order swapped": func(s string) string {
				return strings.Replace(s, "1. The architect DESIGN.\n2. The pre-build design audit", "1. The pre-build design audit.\n2. The architect DESIGN", 1)
			},
		} {
			if p := flowOwnershipProblems(sectionOf(mutate(good), "Who owns control in a flow-managed run")); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}

// regexpFirst is the first match of the pattern in the text ("" when there is none); the checks below use it to name what they found.
func regexpFirst(pattern, text string) string { return regexp.MustCompile(pattern).FindString(text) }

// relayCommandNames is the name of every command of the relay's argument table.
func relayCommandNames(t *testing.T) map[string]bool {
	t.Helper()
	var specs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(readRepoText(t, relaySpecsPath)), &specs); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range specs {
		names[s.Name] = true
	}
	return names
}

var dagCommandWord = regexp.MustCompile("`(dag-[a-z-]+)`")

// runSectionProblems judges the section of crw-run that tells the project orchestrator how a milestone runs as a flow run.
func runSectionProblems(section string, relay map[string]bool) []string {
	flat := normalized(section)
	if flat == "" {
		return []string{"crw-run has no section \"Run a milestone with a flow run\""}
	}
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)one run (of the built-in project lane )?for each milestone`,
		"`crw flow start project-lane ",
		"`--preview`",
		"--handoff",
		`(?i)handles only (two things|escalations)`,
		"`crw flow apply ",
		`(?i)the receipt of an oversized issue`,
		`(?i)never claims the coordinator epoch of a plan (that )?a run holds`,
		"`stale_coordinator_epoch`",
		`(?i)milestone ends .?incomplete`,
		`(?i)out-of-band [^.]*transition[^.]*(is )?(raised|goes) (to the manager )?as an escalation`,
		`(?i)` + "`crw flow resume`",
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	// Every sentence that names the claim command says that the thread does not claim a plan a run holds, or when it may.
	for _, sentence := range regexp.MustCompile(`[.;] `).Split(flat, -1) {
		if strings.Contains(sentence, "`dag-coordinator-claim`") && !regexp.MustCompile(`(?i)\b(never|not|only after|no run holds)\b`).MatchString(sentence) {
			problems = append(problems, "a sentence names `dag-coordinator-claim` without saying when it does not apply: "+sentence)
		}
	}
	// Every relay command the section names is a command the relay has.
	for _, m := range dagCommandWord.FindAllStringSubmatch(flat, -1) {
		if !relay[m[1]] {
			problems = append(problems, "the section names `"+m[1]+"`, which the relay's command table does not have")
		}
	}
	return problems
}

func TestRunProjectNeverClaimsRunEpoch(t *testing.T) {
	relay := relayCommandNames(t)
	if !relay["dag-coordinator-claim"] {
		t.Fatal("the relay's command table has no dag-coordinator-claim: the rule this test holds has no subject")
	}
	section := sectionOf(readRepoText(t, runSkillFile), "Run a milestone with a flow run")
	if problems := runSectionProblems(section, relay); len(problems) > 0 {
		t.Errorf("%s:\n  %s\n%s", runSkillFile, strings.Join(problems, "\n  "), normalized(section))
	}
	// The engine really claims the epoch before its first fenced write, which is what the section protects.
	script := normalized(readRepoText(t, "docs/flow/script.md"))
	if !strings.Contains(script, "Before its first fenced write the run claims the plan's coordinator epoch") {
		t.Error("docs/flow/script.md no longer says that the run claims the plan's coordinator epoch before its first fenced write; revisit the section of crw-run")
	}

	t.Run("fixture: a thread that claims the plan of a run is reported", func(t *testing.T) {
		good := "## Run a milestone with a flow run\n\nThe orchestrator starts one run for each milestone with `crw flow start project-lane --handoff F` after `--preview`. " +
			"The thread handles only two things: an escalation, and the receipt of an oversized issue with `crw flow apply --event E`. " +
			"It never claims the coordinator epoch of a plan a run holds with `dag-coordinator-claim`: the run's next write is `stale_coordinator_epoch` and the milestone ends `incomplete`. " +
			"An out-of-band transition is raised as an escalation. A dead run is taken again with `crw flow resume`.\n"
		fixture := map[string]bool{"dag-coordinator-claim": true}
		if p := runSectionProblems(sectionOf(good, "Run a milestone with a flow run"), fixture); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(string) string{
			"claims":      func(s string) string { return strings.Replace(s, "never claims", "claims", 1) },
			"unknown dag": func(s string) string { return s + "Then `dag-nonesuch`.\n" },
			"no escalate": func(s string) string {
				return strings.Replace(s, "An out-of-band transition is raised as an escalation.", "", 1)
			},
			"no section":    func(s string) string { return strings.Replace(s, "## Run a milestone with a flow run", "## Other", 1) },
			"no apply":      func(s string) string { return strings.Replace(s, "`crw flow apply --event E`", "a command", 1) },
			"unconditional": func(s string) string { return s + "Run `dag-coordinator-claim` at the start of every turn.\n" },
		} {
			if p := runSectionProblems(sectionOf(mutate(good), "Run a milestone with a flow run"), fixture); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}

const (
	issueBoundaries   = "plugins/crw/skills/crw-plan/references/issue-boundaries.md"
	compileSourceFile = "internal/flow/compile/compile.go"
)

var compileRuleWord = regexp.MustCompile("`(over_64|cycle|self_reference|region_order_missing|group_cap|after_unknown|size_value|milestone|input)`")

// closingCheckProblems judges the "Close the plan" section of the issue-boundaries page: the compile of the handoff is its step after
// the read-back, a refusal sends the planner back to the step that owns the defect, and each refusal rule it names is a rule the
// compiler has.
func closingCheckProblems(section, compileSource string) []string {
	var problems []string
	steps := regexp.MustCompile(`(?m)^[0-9]+\. ([A-Z][A-Za-z-]*)\.`).FindAllStringSubmatch(section, -1)
	var names []string
	for _, s := range steps {
		names = append(names, s[1])
	}
	want := []string{"Coverage", "Duplication", "Relations", "Read-back", "Compile"}
	if !slices.Equal(names, want) {
		problems = append(problems, "the steps are "+strings.Join(names, ", ")+", want "+strings.Join(want, ", "))
	}
	flat := normalized(section)
	for _, p := range wordingMissing(flat, []string{
		"`crw flow compile --dry-run`",
		`(?i)exits? 0`,
		`(?i)a refusal is a defect of the plan`,
		`(?i)the plan is not closed (with|while) a refusal`,
		"`size_unreviewed`",
		`(?i)draft-only[^.]*steps 1 to 3 and 5`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	for _, m := range compileRuleWord.FindAllStringSubmatch(flat, -1) {
		if !strings.Contains(compileSource, `"`+m[1]+`"`) {
			problems = append(problems, "the section names the rule `"+m[1]+"`, which "+compileSourceFile+" does not write")
		}
	}
	for _, rule := range []string{"over_64", "cycle", "region_order_missing", "group_cap", "size_value"} {
		if !strings.Contains(flat, "`"+rule+"`") {
			problems = append(problems, "the section does not name the refusal `"+rule+"`")
		}
	}
	return problems
}

func TestPlanClosingCheckRunsCompile(t *testing.T) {
	boundaries := readRepoText(t, issueBoundaries)
	compileSource := readRepoText(t, compileSourceFile)
	if problems := closingCheckProblems(sectionOf(boundaries, "Close the plan"), compileSource); len(problems) > 0 {
		t.Errorf("%s, Close the plan:\n  %s", issueBoundaries, strings.Join(problems, "\n  "))
	}
	// The planning skill closes with the same step, and writes the handoff that compile reads.
	plan := normalized(readRepoText(t, planSkillFile))
	if missing := wordingMissing(plan, []string{
		`(?i)closing check.{0,700}crw flow compile`,
		"`crw-flow-compile-input/1`",
		"`crw flow compile --dry-run`",
	}); len(missing) > 0 {
		t.Errorf("%s lacks: %s", planSkillFile, strings.Join(missing, "\n  "))
	}
	// The handoff's schema name is the one the compiler reads.
	if !strings.Contains(readRepoText(t, "internal/flow/compile/input.go"), `"crw-flow-compile-input/1"`) {
		t.Error("internal/flow/compile/input.go no longer reads crw-flow-compile-input/1; revisit the plan skill")
	}

	t.Run("fixture: the step, its order and the refusals are each required", func(t *testing.T) {
		good := "## Close the plan\n\n1. Coverage. a\n2. Duplication. b\n3. Relations. c\n4. Read-back. d\n5. Compile. Run `crw flow compile --dry-run` over the handoff; it exits 0. A refusal is a defect of the plan: `over_64`, `cycle`, `region_order_missing`, `group_cap`, `size_value`. The plan is not closed with a refusal. `size_unreviewed` is not a refusal.\n\n" +
			"A draft-only request runs steps 1 to 3 and 5.\n"
		source := `"over_64" "cycle" "region_order_missing" "group_cap" "size_value"`
		if p := closingCheckProblems(sectionOf(good, "Close the plan"), source); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(string) string{
			"no step": func(s string) string { return strings.Replace(s, "5. Compile.", "5. Other.", 1) },
			"before": func(s string) string {
				return strings.Replace(s, "4. Read-back. d\n5. Compile.", "4. Compile.\n5. Read-back.", 1)
			},
			"no command":    func(s string) string { return strings.Replace(s, "`crw flow compile --dry-run`", "the compiler", 1) },
			"no rule":       func(s string) string { return strings.Replace(s, "`group_cap`, ", "", 1) },
			"closed anyway": func(s string) string { return strings.Replace(s, "The plan is not closed with a refusal.", "", 1) },
			"foreign rule":  func(s string) string { return strings.Replace(s, "`cycle`", "`self_reference`", 1) },
		} {
			if p := closingCheckProblems(sectionOf(mutate(good), "Close the plan"), source); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}

// The planning skills write what the run reads: the size fields, the group, the proposal of `crw skill size-fit` read as advice, and the
// Orchestration section of the project body.
func TestPlanSizeFieldsAndOrchestrationWording(t *testing.T) {
	plan := readRepoText(t, planSkillFile)
	fields := normalized(sectionOf(plan, "Set the size fields"))
	if missing := wordingMissing(fields, []string{
		"`crw skill issue-size check`", "`crw skill size-fit`",
		`(?i)never divide an issue, or send it to a child, because a number crossed its line`,
		`(?i)the concept boundary decides`,
		"`over_line_accepted`", `(?i)bundle reason`,
		"`size_split`", "`region_order_missing`",
		`(?i)no model name and no effort`,
		fmt.Sprintf(`(?i)a name, at most %d issues`, compileMaxGroup(t)),
	}); len(missing) > 0 {
		t.Errorf("Set the size fields lacks: %s\n%s", strings.Join(missing, "\n  "), fields)
	}
	orchestration := normalized(sectionOf(plan, "Write the Orchestration section of the project"))
	if missing := wordingMissing(orchestration, []string{
		"`## Orchestration`", `(?i)parallel width`, `(?i)region map`, `(?i)integration target`, `(?i)expected review rounds`, `(?i)names no model and no effort`,
	}); len(missing) > 0 {
		t.Errorf("the Orchestration section lacks: %s\n%s", strings.Join(missing, "\n  "), orchestration)
	}
	add := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-add-issue/SKILL.md"), "Set the size fields"))
	if missing := wordingMissing(add, []string{
		"`size_fit`", "`size_reason`", "`issue_size`", "`group`", "`crw skill size-fit`", `(?i)no count alone divides the issue or sends it to a child`,
		`\(\.\./crw-plan/SKILL\.md#set-the-size-fields\)`,
	}); len(missing) > 0 {
		t.Errorf("crw-add-issue, Set the size fields, lacks: %s\n%s", strings.Join(missing, "\n  "), add)
	}
}

// compileMaxGroup is the most issues one group names, read from the compiler's source.
func compileMaxGroup(t *testing.T) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*MaxGroup\s*=\s*(\d+)`).FindStringSubmatch(readRepoText(t, "internal/flow/compile/input.go"))
	if m == nil {
		t.Fatal("internal/flow/compile/input.go has no MaxGroup")
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
