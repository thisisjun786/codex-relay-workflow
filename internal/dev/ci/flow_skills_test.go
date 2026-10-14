//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The skills that describe the flow engine (CRW-1222, the second skill series): the page of the commands (crw-canon/references/flow.md),
// the child's procedure for a flow run (crw-run), the parent's reading of milestone reports, the size fields of
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
		// The run releases the child of an oversized issue; the thread sends it no task packet and no PABCD is armed.
		`(?i)releases? the (issue )?child of an oversized issue itself`,
		"`issue-lane`",
		`(?i)sends? it no task packet`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	if regexp.MustCompile(`(?i)is the path of an oversized issue`).MatchString(flat) {
		problems = append(problems, "the section sends an oversized issue to the dispatch of an issue child, which a run does not use")
	}
	// A thread never hands a decision up through a command the binary does not have.
	if strings.Contains(flat, "crw manage decisions") {
		problems = append(problems, "the section names `crw manage decisions`, which the binary does not have")
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
			"An out-of-band transition is raised as an escalation. A dead run is taken again with `crw flow resume`. " +
			"The run releases the child of an oversized issue itself: it runs the `issue-lane` workflow and the thread sends it no task packet.\n"
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
			"child path":    func(s string) string { return s + "The rest of this skill is the path of an oversized issue.\n" },
			"no packet note": func(s string) string {
				return strings.Replace(s, "and the thread sends it no task packet", "and the thread sends it a packet", 1)
			},
			"unknown command": func(s string) string { return s + "Hand it up with `crw manage decisions`.\n" },
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
	for _, rule := range []string{"input", "over_64", "cycle", "region_order_missing", "group_cap", "size_value"} {
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
		good := "## Close the plan\n\n1. Coverage. a\n2. Duplication. b\n3. Relations. c\n4. Read-back. d\n5. Compile. Run `crw flow compile --dry-run` over the handoff; it exits 0. A refusal is a defect of the plan: `input`, `over_64`, `cycle`, `region_order_missing`, `group_cap`, `size_value`. The plan is not closed with a refusal. `size_unreviewed` is not a refusal.\n\n" +
			"A draft-only request runs steps 1 to 3 and 5.\n"
		source := `"input" "over_64" "cycle" "region_order_missing" "group_cap" "size_value"`
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
			"no input rule": func(s string) string { return strings.Replace(s, "`input`, ", "", 1) },
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

// compileJSONKeys is the JSON keys of the handoff structs of the compiler, by struct name, read from its source.
func compileJSONKeys(t *testing.T) map[string][]string {
	t.Helper()
	src := readRepoText(t, "internal/flow/compile/input.go")
	keys := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?s)type (Input|Project|Milestone|Target|Issue) struct \{(.*?)\n\}`).FindAllStringSubmatch(src, -1) {
		for _, f := range regexp.MustCompile("json:\"([a-z_]+)").FindAllStringSubmatch(m[2], -1) {
			keys[m[1]] = append(keys[m[1]], f[1])
		}
	}
	if len(keys) != 5 {
		t.Fatalf("internal/flow/compile/input.go: read the keys of %d of 5 handoff structs", len(keys))
	}
	return keys
}

// handoffKeyProblems judges the paragraph of crw-plan that spells the handoff document: every JSON key it writes in a code span is a key
// of the compiler, and every key the compiler reads is written unless the planner never sets it.
func handoffKeyProblems(section string, keys map[string][]string, unset []string) []string {
	var problems []string
	flat := normalized(section)
	if flat == "" {
		return []string{"crw-plan has no section \"Hand the plan to the run\""}
	}
	known := map[string]bool{}
	for _, ks := range keys {
		for _, k := range ks {
			known[k] = true
		}
	}
	written := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z][a-z_]*)`").FindAllStringSubmatch(flat, -1) {
		written[m[1]] = true
	}
	for _, k := range slices.Sorted(maps.Keys(known)) {
		if !written[k] && !slices.Contains(unset, k) {
			problems = append(problems, "the section does not name the key `"+k+"`")
		}
	}
	for _, m := range regexp.MustCompile("`([a-z]+_[a-z_]+)`").FindAllStringSubmatch(flat, -1) {
		// a snake_case word in a code span of this section is a key of the document
		if !known[m[1]] {
			problems = append(problems, "the section writes `"+m[1]+"`, which is no key of the handoff the compiler reads")
		}
	}
	for _, p := range wordingMissing(flat, []string{"`crw-flow-compile-input/1`", `(?i)unknown key is refused`, "`schema` is required", "`crw flow compile --dry-run FILE`"}) {
		problems = append(problems, "the section lacks "+p)
	}
	return problems
}

func TestPlanHandoffNamesTheCompilerKeys(t *testing.T) {
	keys := compileJSONKeys(t)
	unset := []string{"request_id", "coordinator_epoch", "kind", "criteria_set_digest"}
	// the issue's description and labels (CRW-1328) are read by crw flow start's issue_ready fact, never by the compiler, and crw-plan
	// tells the planner to write them (CRW-1382): the section names every key of the compile input but the unset ones
	section := sectionOf(readRepoText(t, planSkillFile), "Hand the plan to the run")
	if problems := handoffKeyProblems(section, keys, unset); len(problems) > 0 {
		t.Errorf("%s, Hand the plan to the run:\n  %s\n%s", planSkillFile, strings.Join(problems, "\n  "), normalized(section))
	}
	t.Run("fixture: a misspelled key and a missing key are reported", func(t *testing.T) {
		fk := map[string][]string{"Input": {"schema", "projects"}, "Issue": {"size_fit", "edit_regions_real"}}
		good := "## Hand the plan to the run\n\nSchema `crw-flow-compile-input/1`; an unknown key is refused and `schema` is required. Keys: `projects`, `size_fit`, `edit_regions_real`. Check with `crw flow compile --dry-run FILE`.\n"
		if p := handoffKeyProblems(good, fk, nil); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		bad := strings.Replace(good, "`edit_regions_real`", "`edit_regions`", 1)
		if p := handoffKeyProblems(bad, fk, nil); len(p) != 2 {
			t.Errorf("a misspelled key is reported as %v", p)
		}
		if p := handoffKeyProblems(strings.Replace(good, "`projects`, ", "", 1), fk, nil); len(p) != 1 {
			t.Errorf("a missing key is reported as %v", p)
		}
		if p := handoffKeyProblems(strings.Replace(good, "`schema` is required", "schema is optional", 1), fk, nil); len(p) == 0 {
			t.Error("a handoff that does not require the schema is accepted")
		}
	})
}

// A task packet belongs to an issue child of a plan with no run; the child a run releases runs a workflow and starts no PABCD state machine.
func taskPacketScopeProblems(intro string) []string {
	flat := normalized(intro)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)packets? (below )?(is|are) for an issue child of a plan with no run`,
		"`issue-lane`",
		`(?i)(sends|gets)( it)? no (task )?packet`,
		`(?i)no PABCD state machine`,
		`flow\.md#who-owns-control-in-a-flow-managed-run`,
	}) {
		problems = append(problems, "the introduction lacks "+p)
	}
	return problems
}

func TestTaskPacketIsForAPlanWithNoRun(t *testing.T) {
	text := readRepoText(t, "plugins/crw/skills/crw-run/references/task-packet.md")
	intro := text
	if i := strings.Index(text, "\n## Child task titles"); i > 0 {
		intro = text[:i]
	}
	if problems := taskPacketScopeProblems(intro); len(problems) > 0 {
		t.Errorf("task-packet.md introduction:\n  %s\n%s", strings.Join(problems, "\n  "), normalized(intro))
	}
	good := "The packets are for an issue child of a plan with no run. A node a run releases runs `issue-lane`, gets no packet from the thread and starts no PABCD state machine ([control](../../crw-canon/references/flow.md#who-owns-control-in-a-flow-managed-run))."
	if p := taskPacketScopeProblems(good); len(p) > 0 {
		t.Fatalf("the good text is reported: %v", p)
	}
	if p := taskPacketScopeProblems(strings.Replace(good, "gets no packet from the thread and ", "", 1)); len(p) == 0 {
		t.Error("a text that does not say the run's child gets no packet is accepted")
	}
}

var skillManageCommand = regexp.MustCompile("`crw manage ([a-z][a-z-]*)")

// manageHelpSubcommands is the subcommands `crw manage --help` lists, one for each indented line.
func manageHelpSubcommands(help string) []string {
	var subs []string
	for _, line := range lines(help) {
		if m := regexp.MustCompile(`^  ([a-z][a-z-]*)(\s|$)`).FindStringSubmatch(line); m != nil {
			subs = append(subs, m[1])
		}
	}
	return subs
}

// skillManageProblems compares the `crw manage <sub>` the skills write in code spans with the ones the help lists.
func skillManageProblems(texts map[string]string, help string) []string {
	listed := manageHelpSubcommands(help)
	if len(listed) == 0 {
		return []string{"the help lists no subcommand"}
	}
	var problems []string
	for _, file := range slices.Sorted(maps.Keys(texts)) {
		for _, m := range skillManageCommand.FindAllStringSubmatch(texts[file], -1) {
			if !slices.Contains(listed, m[1]) {
				problems = append(problems, "`crw manage "+m[1]+"` is written in "+file+" and the binary does not list it (`crw manage --help`: "+strings.Join(listed, ", ")+")")
			}
		}
	}
	return problems
}

func TestSkillManageCommandsExist(t *testing.T) {
	root := repoRoot()
	texts := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "plugins/crw/skills"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		texts[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(testsupport.CRW(t), "manage", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("crw manage --help: %v\n%s", err, out)
	}
	if problems := skillManageProblems(texts, string(out)); len(problems) > 0 {
		t.Errorf("%s", strings.Join(problems, "\n"))
	}
	t.Run("fixture: a subcommand the binary lacks is reported", func(t *testing.T) {
		help := "usage: crw manage [-h] {audit,batch}\n  audit\tgrade\n  batch\tselect\n"
		if p := skillManageProblems(map[string]string{"a.md": "Run `crw manage batch` and `crw manage audit --x`."}, help); len(p) > 0 {
			t.Errorf("listed subcommands are reported: %v", p)
		}
		if p := skillManageProblems(map[string]string{"b.md": "Run `crw manage decisions`."}, help); len(p) != 1 {
			t.Errorf("an unknown subcommand is reported as %v", p)
		}
	})
}

const (
	flowCLIFile      = "internal/flow/cli/cli.go"
	hierarchyRefFile = canonRefDir + "hierarchy.md"
)

// flowUsageHas reports whether `crw flow --help` (the Usage text of internal/flow/cli) lists the command line.
func flowUsageHas(t *testing.T, line string) bool {
	t.Helper()
	return strings.Contains(readRepoText(t, flowCLIFile), line)
}

// operatingFlowCommand is a `crw flow` command that changes a run: the parent and the status reader do not run one.
var operatingFlowCommand = regexp.MustCompile("`crw flow (start|answer|pause|cancel|resume|recover|apply|report-up|report-delivered|digest)\\b")

// initiativeReportProblems judges the subsection of the parent's section of crw-run that reads milestone reports; whole is
// the parent's section (CRW-1352: crw-run-initiative, the initiative lead's skill, was folded into it).
func initiativeReportProblems(section, whole string) []string {
	flat := normalized(section)
	if flat == "" {
		return []string{"the parent's section has no subsection \"Read the milestone reports\""}
	}
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		"`crw flow reports --initiative INITIATIVE --unread`",
		`(?i)at the start of every turn`,
		`(?i)waits in the supervisor queue until the thread loads`,
		"`milestone_report`", "`project_blocked`", "`project_decision_request`",
		"\\[crw-check\\]\\(\\.\\./crw-check/SKILL\\.md\\)",
		"`crw flow report-ack REPORT --turn TURN`",
		`(?i)acknowledges a report only after it has judged it`,
		`(?i)reads and acknowledges[^.]*(never|does not)[^.]*(operate|act on)[^.]*run|never operates? a run`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	if found := operatingFlowCommand.FindString(whole); found != "" {
		problems = append(problems, "the parent's section names "+found+"`, a command that changes a run, which the parent does not run")
	}
	return problems
}

// The parent starts the child of a project when the project's prerequisites hold and the cap on children allows it (the parent
// owns the caps, `crw manage capacity`).
func TestRunInitiativeStartsOrchestratorUnderCap(t *testing.T) {
	_, parent := runSkillParts(t)
	if !strings.Contains(normalized(parent), "only when its prerequisites hold and the cap on children that `crw manage capacity` judges allows it") {
		t.Errorf("the parent's section of %s does not say that a project's child starts only when its prerequisites hold and the cap allows it", runSkillFile)
	}
}

func TestRunInitiativeReadsReports(t *testing.T) {
	_, text := runSkillParts(t)
	if problems := initiativeReportProblems(sectionOf(text, "Read the milestone reports"), text); len(problems) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(problems, "\n  "))
	}
	// The commands are the commands of the binary's usage, with the flags the skill uses.
	for _, line := range []string{"crw flow reports [--initiative I] [--unread] [--json]", "crw flow report-ack REPORT --turn T"} {
		if !flowUsageHas(t, line) {
			t.Errorf("internal/flow/cli/cli.go no longer has the usage line %q", line)
		}
	}

	t.Run("fixture: a parent that operates a run or skips the ack is reported", func(t *testing.T) {
		good := "## Read the milestone reports\n\nRun `crw flow reports --initiative INITIATIVE --unread` at the start of every turn: a report to a thread that is not loaded waits in the supervisor queue until the thread loads. " +
			"The kinds are `milestone_report`, `project_blocked` and `project_decision_request`. Judge each with [crw-check](../crw-check/SKILL.md), then `crw flow report-ack REPORT --turn TURN`: it acknowledges a report only after it has judged it. " +
			"It never operates a run.\n"
		section := sectionOf(good, "Read the milestone reports")
		if p := initiativeReportProblems(section, good); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		if p := initiativeReportProblems(section, good+"Then `crw flow resume RUN`.\n"); len(p) == 0 {
			t.Error("a skill that resumes a run is accepted")
		}
		for label, mutate := range map[string]func(string) string{
			"no unread": func(s string) string { return strings.Replace(s, " --unread", "", 1) },
			"no ack": func(s string) string {
				return strings.Replace(s, "`crw flow report-ack REPORT --turn TURN`", "a mark", 1)
			},
			"no check":  func(s string) string { return strings.Replace(s, "[crw-check](../crw-check/SKILL.md)", "a look", 1) },
			"each turn": func(s string) string { return strings.Replace(s, "at the start of every turn", "sometimes", 1) },
		} {
			if p := initiativeReportProblems(sectionOf(mutate(good), "Read the milestone reports"), mutate(good)); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}

// statusFlowProblems judges how crw-status reads a run: with the read commands, and without a command that writes.
func statusFlowProblems(section, whole string) []string {
	flat := normalized(section)
	if flat == "" {
		return []string{"crw-status has no section \"Read a project's run\""}
	}
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		"`crw flow ps`", "`crw flow show RUN`", "`crw flow watch RUN --after SEQ`", "`crw relay journal-tail`",
		"`crw flow reports --unread`",
		`(?i)unread report`,
		`(?i)(does|do) not acknowledge[^.]*` + "`crw flow report-ack`",
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	if found := operatingFlowCommand.FindString(whole); found != "" {
		problems = append(problems, "the skill names "+found+"`, a command that changes a run, in a read-only report")
	}
	return problems
}

func TestStatusReadsRunStateAndUnreadReports(t *testing.T) {
	text := readRepoText(t, statusSkillFile)
	if problems := statusFlowProblems(sectionOf(text, "Read a project's run"), text); len(problems) > 0 {
		t.Errorf("%s:\n  %s", statusSkillFile, strings.Join(problems, "\n  "))
	}
	for _, line := range []string{"crw flow ps [--all] [--json]", "crw flow show RUN [--json]", "crw flow watch RUN [--after SEQ]", "crw flow reports [--initiative I] [--unread]"} {
		if !flowUsageHas(t, line) {
			t.Errorf("internal/flow/cli/cli.go no longer has the usage line %q", line)
		}
	}
	if !strings.Contains(readRepoText(t, "internal/relay/argparse/specs.json"), `"journal-tail"`) {
		t.Error("the relay's command table has no journal-tail")
	}
}

// The flow run's issue executor (CRW-1222) is a subagent of the level table since CRW-1352: no layer, no Codex task of its own,
// no relay role id, and the run alone speaks to it.
func TestHierarchyNamesTheIssueExecutor(t *testing.T) {
	text := readRepoText(t, hierarchyRefFile)
	rows := tableRows(text, "Korean")
	var subagents []string
	for _, row := range rows {
		if len(row) > 2 && strings.HasPrefix(row[1], "subagents") {
			subagents = row
		}
	}
	if subagents == nil {
		t.Fatalf("the level table of %s has no subagents row: %v", hierarchyRefFile, rows)
	}
	if relayRoleID.MatchString(subagents[2]) || !strings.Contains(subagents[1], "executor") {
		t.Errorf("the subagents row names a relay role id or no executor: %q", subagents)
	}
	flat := normalized(text)
	if missing := wordingMissing(flat, []string{
		`(?i)subagent[^.]*(is not|holds no)[^.]*(Codex task|binding)`,
		`(?i)the run alone (speaks to|instructs) (it|a subagent)`,
	}); len(missing) > 0 {
		t.Errorf("%s lacks: %s", hierarchyRefFile, strings.Join(missing, "\n  "))
	}
}

// skillFlowSubcommands is every `crw flow <sub>` the skill texts write in code (an inline span or a fenced block), with the files that
// write it. `crw flow _run` is the run process's own entry and `crw flow <run>` is a placeholder; neither is a subcommand a reader types.
var skillFlowCommand = regexp.MustCompile("`?crw flow ([a-z][a-z-]*)")

func skillFlowSubcommands(texts map[string]string) map[string][]string {
	found := map[string][]string{}
	for file, text := range texts {
		fence := ""
		for _, line := range lines(text) {
			trimmed := strings.TrimLeft(line, " ")
			if marker := fenceMarker(trimmed); marker != "" {
				switch {
				case fence == "":
					fence = marker
				case marker[0] == fence[0] && len(marker) >= len(fence):
					fence = ""
				}
				continue
			}
			scan := line
			if fence == "" {
				// outside a fence only the inline code spans count
				scan = strings.Join(inlineCode.FindAllString(line, -1), " ")
			}
			for _, m := range skillFlowCommand.FindAllStringSubmatch(scan, -1) {
				if !slices.Contains(found[m[1]], file) {
					found[m[1]] = append(found[m[1]], file)
				}
			}
		}
	}
	return found
}

// flowHelpSubcommands is the subcommands the output of `crw flow --help` lists: the word after `crw flow` at the start of a usage line.
func flowHelpSubcommands(help string) []string {
	var subs []string
	for _, line := range lines(help) {
		if m := regexp.MustCompile(`^(?:usage: )?\s*crw flow ([a-z][a-z-]*)`).FindStringSubmatch(line); m != nil && !slices.Contains(subs, m[1]) {
			subs = append(subs, m[1])
		}
	}
	slices.Sort(subs)
	return subs
}

// skillCommandProblems compares the subcommands the skills write with the ones the help lists.
func skillCommandProblems(texts map[string]string, help string) []string {
	var problems []string
	listed := flowHelpSubcommands(help)
	if len(listed) == 0 {
		return []string{"the help lists no subcommand"}
	}
	found := skillFlowSubcommands(texts)
	for _, sub := range slices.Sorted(maps.Keys(found)) {
		if !slices.Contains(listed, sub) {
			problems = append(problems, "`crw flow "+sub+"` is written in "+strings.Join(found[sub], ", ")+" and the binary does not list it (`crw flow --help`: "+strings.Join(listed, ", ")+")")
		}
	}
	return problems
}

func TestSkillCommandsExist(t *testing.T) {
	root := repoRoot()
	texts := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "plugins/crw/skills"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		texts[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(testsupport.CRW(t), "flow", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("crw flow --help: %v\n%s", err, out)
	}
	if problems := skillCommandProblems(texts, string(out)); len(problems) > 0 {
		t.Errorf("%s", strings.Join(problems, "\n"))
	}
	// The commands the skills of this series rely on are among the ones written, so the comparison above has them as its subject.
	found := skillFlowSubcommands(texts)
	for _, sub := range []string{"start", "show", "watch", "apply", "reports", "report-ack", "report-up", "compile", "resume", "answer"} {
		if len(found[sub]) == 0 {
			t.Errorf("no skill text writes `crw flow %s`: the comparison no longer covers it", sub)
		}
	}

	t.Run("fixture: a command the binary lacks is reported, the ones it lists and the placeholders are not", func(t *testing.T) {
		help := "usage: crw flow start WORKFLOW\n       crw flow ps [--all]\n       crw flow trace export --out F\n"
		ok := map[string]string{"a.md": "Run `crw flow start x` and `crw flow trace export`.\n\n```\ncrw flow ps\n```\nThe `crw flow <run> line` and `crw flow _run` are not commands. The crw flow command table is prose.\n"}
		if p := skillCommandProblems(ok, help); len(p) > 0 {
			t.Errorf("listed commands are reported: %v", p)
		}
		bad := map[string]string{"b.md": "Run `crw flow nonesuch --x`.\n\n```\ncrw flow gone\n```\n"}
		p := skillCommandProblems(bad, help)
		if len(p) != 2 || !strings.Contains(p[0], "crw flow gone") || !strings.Contains(p[1], "crw flow nonesuch") {
			t.Errorf("the unknown commands are not reported: %v", p)
		}
		if p := skillCommandProblems(ok, "no usage here\n"); len(p) == 0 {
			t.Error("an empty help is accepted")
		}
	})
}
