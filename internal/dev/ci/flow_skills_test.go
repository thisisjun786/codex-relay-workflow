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
		`(?i)the crosscheck does not replace the critic's design check`,
		`(?i)crosscheck[^.]*separate (tool )?invocation`,
		`(?i)same.thread[^.]*warning[^.]*record[^.]*continue`,
		`(?i)tool review[^.]*no (agent )?thread id`,
		`(?i)before any executor starts`,
		`(?i)needs_changes[^.]*returns to the architect`,
		`(?i)replan[^.]*audited again`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	// The order of the C4 procedure is the order of its numbered steps (CRW-1455): DESIGN, then the critic's design check, then the unit
	// check of each node before its commit, then the final gate with the crosscheck (CRW-1473), then the acceptance audit and the report.
	var steps []string
	for _, line := range lines(section) {
		if numberedStep.MatchString(line) {
			steps = append(steps, normalized(line))
		}
	}
	wantSteps := [][]string{{"architect DESIGN"}, {"critic's design check", "pre-build"}, {"unit check", "before the run commits"}, {"final gate", "crosscheck"}, {"acceptance audit"}, {"report"}}
	if len(steps) != len(wantSteps) {
		problems = append(problems, "the high-risk procedure has "+strconv.Itoa(len(steps))+" numbered steps, want "+strconv.Itoa(len(wantSteps)))
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
	for _, want := range []string{"the critic's separate review execution", "the crosscheck does not replace the critic's design check", "same-thread review is warning-only and recorded; the run continues"} {
		if !strings.Contains(dev, want) {
			t.Errorf("crw-dev/SKILL.md lacks %q", want)
		}
	}
	// The background explorers come first (CRW-1466): one for each node of the milestone, at the design head, before the first DESIGN.
	run := normalized(readRepoText(t, "plugins/crw/skills/crw-run/SKILL.md"))
	if !strings.Contains(run, "The sequence is fixed: one background explorer for each node of the milestone reads the repository at the design head before the first DESIGN, the architect writes the DESIGN over their findings,") {
		t.Errorf("crw-run/SKILL.md does not start the fixed sequence with the background explorers")
	}
	if !strings.Contains(normalized(section), "1. One background explorer for each node of the milestone, at the design head, then the architect DESIGN of the milestone over their findings.") {
		t.Errorf("%s: the C4 procedure does not start with the background explorers", flowRef)
	}
	// The critic's design check is the critic's call (CRW-1434): no installed guidance gives it to a reviewer.
	reviewerAudits := regexp.MustCompile(`(?i)design (audit|check),? by a reviewer|a reviewer of another model family (audits|checks) it|reviewer of another model family for the (design audit|critic's design check)`)
	for _, path := range []string{flowRef, "plugins/crw/skills/crw-dev/SKILL.md", "plugins/crw/skills/crw-run/SKILL.md", "plugins/crw/skills/crw-canon/references/spawn-policy.md"} {
		if m := reviewerAudits.FindString(normalized(readRepoText(t, path))); m != "" {
			t.Errorf("%s gives the design audit to a reviewer: %q", path, m)
		}
	}

	t.Run("fixture: each missing sentence is reported", func(t *testing.T) {
		good := "## Who owns control in a flow-managed run\n\n| Control | Owner |\n|---|---|\n| Phases | the run |\n| Audit | the run |\n| Evidence | the run |\n\n" +
			"The executor and an oversized child start no PABCD state machine. The standalone PABCD rules are unchanged.\n\n" +
			"1. The architect DESIGN.\n2. The critic's design check, pre-build, by the critic in a separate review execution, before any executor starts; needs_changes returns to the architect and a replan is audited again.\n" +
			"3. The unit check of each node before the run commits it.\n4. After integration the final gate, then the crosscheck over the integrated range in a separate tool invocation.\n" +
			"A same-thread review, if observed, is warning-only and recorded; the run continues. Tool reviews have no agent thread id.\n5. The acceptance audit.\n6. The report.\n\nThe crosscheck does not replace the critic's design check.\n"
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
				return strings.Replace(s, "The crosscheck does not replace", "The crosscheck replaces", 1)
			},
			"same-thread review handling": func(s string) string {
				return strings.Replace(s, "same-thread review, if observed, is warning-only and recorded; the run continues", "same-thread review is refused", 1)
			},
			"audit after run": func(s string) string {
				return strings.Replace(s, "before any executor starts", "after the executors", 1)
			},
			"order swapped": func(s string) string {
				return strings.Replace(s, "1. The architect DESIGN.\n2. The critic's design check", "1. The critic's design check.\n2. The architect DESIGN", 1)
			},
			"no acceptance audit": func(s string) string { return strings.Replace(s, "5. The acceptance audit.\n6.", "5.", 1) },
			"unit check after the commit": func(s string) string {
				return strings.Replace(s, "unit check of each node before the run commits it", "unit check of each node after the commit", 1)
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
		`(?i)never claims the coordinator epoch of a plan (that )?a run holds`,
		"`stale_coordinator_epoch`",
		`(?i)milestone ends .?incomplete`,
		`(?i)out-of-band [^.]*transition[^.]*(is )?(raised|goes) (to the manager )?as an escalation`,
		`(?i)` + "`crw flow resume`",
		// A node sized for a `child` session starts no `child` session (CRW-1415): the project lane escalates it as size_split with detail
		// child_retired, the parent asks for a bundle split, and the thread sends no task packet for it.
		`(?i)starts? no \x60child\x60 session`,
		"`size_split`",
		"`child_retired`",
		`(?i)bundle split`,
		`(?i)sends? (it )?no task packet`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	if m := regexp.MustCompile("(?i)releases? the (issue )?child of an oversized issue|crw\\.releaseChild|`issue-lane`").FindString(flat); m != "" {
		problems = append(problems, "the section says the run starts a `child` session ("+m+"), a path the project lane retired")
	}
	if regexp.MustCompile(`(?i)is the path of an oversized issue`).MatchString(flat) {
		problems = append(problems, "the section sends an oversized issue to the dispatch of a `child` session, which a run does not use")
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
			"The thread handles only escalations. " +
			"It never claims the coordinator epoch of a plan a run holds with `dag-coordinator-claim`: the run's next write is `stale_coordinator_epoch` and the milestone ends `incomplete`. " +
			"An out-of-band transition is raised as an escalation. A dead run is taken again with `crw flow resume`. " +
			"A node sized for a `child` session starts no `child` session: the project lane escalates it as `size_split` with detail `child_retired`, the parent asks for a bundle split, and the thread sends no task packet for it.\n"
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
			"unconditional": func(s string) string { return s + "Run `dag-coordinator-claim` at the start of every turn.\n" },
			"child path":    func(s string) string { return s + "The rest of this skill is the path of an oversized issue.\n" },
			"no packet note": func(s string) string {
				return strings.Replace(s, "and the thread sends no task packet for it", "and the thread sends it a packet", 1)
			},
			"no child_retired": func(s string) string { return strings.Replace(s, " with detail `child_retired`", "", 1) },
			"no bundle split":  func(s string) string { return strings.Replace(s, "the parent asks for a bundle split, ", "", 1) },
			"retired child path": func(s string) string {
				return s + "The run releases the child of an oversized issue itself: it runs the `issue-lane` workflow.\n"
			},
			"unknown command": func(s string) string { return s + "Hand it up with `crw manage decisions`.\n" },
		} {
			if p := runSectionProblems(sectionOf(mutate(good), "Run a milestone with a flow run"), fixture); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}

// CRW-1437 (S-2): a node the architect sized `bundle_split` starts no executor; the project lane escalates it as size_split with ask
// bundle_split_request, the escalation notice names the request, and the child asks the parent for the bundle split of the node's issue
// with `crw flow report-up --kind project_decision_request`. The child's procedure (crw-run), the reporting page and the spawn policy say
// so, and the request the notice names is the one report/hook.go writes.

const reportHookFile = "internal/flow/report/hook.go"

// bundleSplitRunProblems judges the section of crw-run on a flow run for the bundle split request of a node sized `bundle_split`.
func bundleSplitRunProblems(section string) []string {
	flat := normalized(section)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		"`bundle_split`",
		"`size_split` with ask `bundle_split_request`",
		`(?i)starts? no executor`,
		`(?i)waits? for no (answer|decision)`,
		`(?i)evidence file[^.]*the issue[^.]*the node[^.]*\x60size_reason\x60`,
		"`crw flow report-up --kind project_decision_request --run RUN --evidence FILE`",
		`(?i)the parent \(repository session\) makes the bundle split of the node's issue`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	if m := regexp.MustCompile("(?i)report-up --kind (project_blocked|jun_decision_request)[^.]*bundle split|bundle split[^.]*report-up --kind (project_blocked|jun_decision_request)").FindString(flat); m != "" {
		problems = append(problems, "the section asks for a bundle split by a report kind other than project_decision_request ("+m+")")
	}
	return problems
}

// bundleSplitReportingProblems judges docs/flow/reporting.md for the escalation that asks bundle_split_request.
func bundleSplitReportingProblems(text string) []string {
	var problems []string
	head := text
	if i := strings.Index(text, "\n## "); i >= 0 {
		head = text[:i]
	}
	table := normalized(head) // the table of what goes up stands before the first section
	for _, p := range wordingMissing(table, []string{
		"`size_split`[^|]*`bundle_split_request`",
		`(?i)\| a project-level blocker or decision request[^|]*bundle split`,
	}) {
		problems = append(problems, "the table lacks "+p)
	}
	notices := normalized(sectionOf(text, "Escalations and the completion digest"))
	for _, p := range wordingMissing(notices, []string{
		"`The run waits for your decision \\(<ask>\\).`",
		"`bundle_split_request`[^.]*in place of",
		"`crw flow report-up --kind project_decision_request --run RUN --evidence FILE`",
		`(?i)the parent's to make`,
	}) {
		problems = append(problems, "the escalation section lacks "+p)
	}
	return problems
}

// bundleSplitSpawnProblems judges the spawn policy: SP-EXEC-1 spawns no executor for a node sized `bundle_split`, and SP-TIER-2 sends the
// node to a bundle split, not to a `split`, which since CRW-1435 is a node of sub-tasks.
func bundleSplitSpawnProblems(text string) []string {
	row := func(id string) string {
		for _, line := range lines(text) {
			if strings.HasPrefix(strings.TrimSpace(line), "| `"+id+"` |") {
				return line
			}
		}
		return ""
	}
	var problems []string
	exec := row("SP-EXEC-1")
	for _, p := range wordingMissing(exec, []string{
		"`bundle_split`[^|]*no executor",
		"`size_split` with ask `bundle_split_request`",
	}) {
		problems = append(problems, "the SP-EXEC-1 row lacks "+p)
	}
	tier := row("SP-TIER-2")
	if !regexp.MustCompile("(?i)a bundle split through `crw-add-issue`").MatchString(tier) {
		problems = append(problems, "the SP-TIER-2 row does not send the node to a bundle split through `crw-add-issue`: "+tier)
	}
	return problems
}

func TestRunSkillNamesTheBundleSplitRequest(t *testing.T) {
	section := sectionOf(readRepoText(t, runSkillFile), "Run a milestone with a flow run")
	if p := bundleSplitRunProblems(section); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	if p := bundleSplitReportingProblems(readRepoText(t, "docs/flow/reporting.md")); len(p) > 0 {
		t.Errorf("docs/flow/reporting.md:\n  %s", strings.Join(p, "\n  "))
	}
	if p := bundleSplitSpawnProblems(readRepoText(t, spawnPolicyRef)); len(p) > 0 {
		t.Errorf("%s:\n  %s", spawnPolicyRef, strings.Join(p, "\n  "))
	}
	// The pages name the request the run really writes: the lane's ask and the notice's report-up command.
	hook := readRepoText(t, reportHookFile)
	for _, want := range []string{`"bundle_split_request"`, "crw flow report-up --kind project_decision_request"} {
		if !strings.Contains(hook, want) {
			t.Errorf("%s no longer writes %s; revisit crw-run, reporting.md and the spawn policy", reportHookFile, want)
		}
	}
	if lane := readRepoText(t, "internal/flow/workflows/project-lane.js"); !strings.Contains(lane, "ask: 'bundle_split_request'") {
		t.Error("project-lane.js no longer escalates a bundle_split node with ask bundle_split_request; revisit crw-run, reporting.md and the spawn policy")
	}

	t.Run("fixture: a section without the request is reported", func(t *testing.T) {
		good := "## Run a milestone with a flow run\n\n" +
			"A node the architect sized `bundle_split` starts no executor: the project lane escalates it as `size_split` with ask `bundle_split_request` " +
			"and waits for no answer to it. The parent (repository session) makes the bundle split of the node's issue. This thread writes an evidence file that " +
			"names the issue, the node and the plan's `size_reason`, and runs `crw flow report-up --kind project_decision_request --run RUN --evidence FILE`.\n"
		if p := bundleSplitRunProblems(sectionOf(good, "Run a milestone with a flow run")); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(string) string{
			"no ask":      func(s string) string { return strings.Replace(s, " with ask `bundle_split_request`", "", 1) },
			"an executor": func(s string) string { return strings.Replace(s, "starts no executor", "starts an executor", 1) },
			"waits":       func(s string) string { return strings.Replace(s, "waits for no answer", "waits for the answer", 1) },
			"no evidence": func(s string) string {
				return strings.Replace(s, "the issue, the node and the plan's `size_reason`", "the node", 1)
			},
			"blocked kind": func(s string) string { return strings.Replace(s, "project_decision_request", "project_blocked", 1) },
			"jun kind": func(s string) string {
				return strings.Replace(s, "project_decision_request", "jun_decision_request", 1)
			},
			"not the parent's": func(s string) string {
				return strings.Replace(s, "The parent (repository session) makes the bundle split of the node's issue. ", "", 1)
			},
		} {
			if p := bundleSplitRunProblems(sectionOf(mutate(good), "Run a milestone with a flow run")); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})

	t.Run("fixture: a spawn policy that sends the node to a split is reported", func(t *testing.T) {
		good := "| `SP-EXEC-1` | an executor | every issue node | A node sized `bundle_split` gets no executor: the lane escalates it as `size_split` with ask `bundle_split_request` |\n" +
			"| `SP-TIER-2` | the tier step is exhausted | again | no spawn; the node escalates (`tiers_exhausted`) to the child: a bundle split through `crw-add-issue` and a plan revision, or a decision for Jun |\n"
		if p := bundleSplitSpawnProblems(good); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(string) string{
			"old split": func(s string) string { return strings.Replace(s, "a bundle split through", "a split through", 1) },
			"executor":  func(s string) string { return strings.Replace(s, "gets no executor", "gets an executor", 1) },
		} {
			if p := bundleSplitSpawnProblems(mutate(good)); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})
}

// A node the architect sized `split` with sub-tasks (CRW-1459) is released to one executor: the sub-tasks run in their order on the node's one
// executor thread, each next one a `subtask:<node>:<tier>:s<k>` turn of that thread; each before the last is committed as a checkpoint against
// its own regions, with no unit check, no verification and no land; the last goes through the node's commit, verification, the one check of
// the node's whole change and land, so the node is judged once. A sub-task that reported failed is named in the next packet and judged with the node; a fresh thread starts at the
// node's last commit and its packet at the first sub-task not done. A split with no sub-tasks (a DESIGN recorded before S-1) still goes up as
// `size_split`. crw-run, the task packet page and the two flow documents say so, and none of them still sends every split up.

// splitRunProblems judges the "Run a milestone with a flow run" section of crw-run for the split node's sub-task loop.
func splitRunProblems(section string) []string {
	flat := normalized(section)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)sized \x60split\x60[^.]*sub-tasks[^.]*released to one executor`,
		`(?i)sub-tasks run in their order on the node's one executor thread`,
		"`subtask:<node>:<tier>:s<k>`",
		`(?i)before the last[^.]*checkpoint[^.]*no unit check, no verification and no land`,
		`(?i)judged once`,
		`(?i)check of the node's whole change[^.]*every criterion of the node[^.]*acceptance rests`,
		`(?i)reported failed[^.]*named in the next[^.]*judged with the node`,
		`(?i)fresh thread[^.]*node's last commit[^.]*first sub-task not done`,
		`(?i)split with no sub-tasks[^.]*\x60size_split\x60`,
		`(?i)(sends|sends it) no task packet for (any )?(a |the )?sub-task`,
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	if m := regexp.MustCompile(`(?i)sizes as a split goes up as|the child divides the plan's node`).FindString(flat); m != "" {
		problems = append(problems, "the section still sends every split up ("+m+")")
	}
	if m := splitSubTaskCheck.FindString(flat); m != "" {
		problems = append(problems, "the section still checks each sub-task ("+m+")")
	}
	return problems
}

// splitSubTaskCheck is wording of a unit check of each sub-task alone, which the run does not make: a split node is checked once, on its
// whole change (a check of each sub-task is CRW-1564's).
var splitSubTaskCheck = regexp.MustCompile(`(?i)unit-checked against that sub-task alone|later sub-tasks are not due yet|later ones as not due yet|opts\.sub\b|row records the \x60sub\x60`)

// splitDocProblems judges a flow document (script.md, journal.md) for the checkpoint commits of a split node.
func splitDocProblems(text string) []string {
	flat := normalized(text)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)split node makes one checkpoint \x60crw\.commit\x60 (for each|per) sub-task before the last`,
		`(?i)is judged once`,
		"`subtask:<node>:<tier>:s<k>`",
		`(?i)whole change`,
	}) {
		problems = append(problems, "the document lacks "+p)
	}
	if m := regexp.MustCompile(`(?i)a node sized for a child or a split is not released to an executor`).FindString(flat); m != "" {
		problems = append(problems, "the document still releases no split to an executor ("+m+")")
	}
	if m := splitSubTaskCheck.FindString(flat); m != "" {
		problems = append(problems, "the document still checks each sub-task ("+m+")")
	}
	return problems
}

// splitPacketProblems judges the introduction of the task packet page: the sub-tasks of a split node are the run's own turns.
func splitPacketProblems(intro string) []string {
	flat := normalized(intro)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)node sized \x60split\x60 with sub-tasks[^.]*one executor thread`,
		"`subtask:<node>:<tier>:s<k>`",
		`(?i)sends no packet for (any of them|a sub-task)`,
	}) {
		problems = append(problems, "the introduction lacks "+p)
	}
	return problems
}

func TestRunSkillSaysASplitRunsSubTasksOnOneExecutor(t *testing.T) {
	if p := splitRunProblems(sectionOf(readRepoText(t, runSkillFile), "Run a milestone with a flow run")); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	packetPage := readRepoText(t, "plugins/crw/skills/crw-run/references/task-packet.md")
	if i := strings.Index(packetPage, "\n## Child task titles"); i > 0 {
		packetPage = packetPage[:i]
	}
	if p := splitPacketProblems(packetPage); len(p) > 0 {
		t.Errorf("task-packet.md introduction:\n  %s", strings.Join(p, "\n  "))
	}
	for _, doc := range []string{"docs/flow/script.md", "docs/flow/journal.md"} {
		if p := splitDocProblems(readRepoText(t, doc)); len(p) > 0 {
			t.Errorf("%s:\n  %s", doc, strings.Join(p, "\n  "))
		}
	}
	// The pages name what the lane really does: the sub-task turn's label, the checkpoint commit against the sub-task's regions, a branch
	// of its own for a split node's receipts and the committed check of the whole change; no check names a sub-task.
	lane := readRepoText(t, "internal/flow/workflows/project-lane.js")
	for _, want := range []string{"label: 'subtask:' + node + ':' + tier + ':s'", "crw.commit(node, ex, sub.edit_regions)", "!(plan.sub_tasks && plan.sub_tasks.length)",
		"} else if (subs.length) {", "commit: head, base: ci.base"} {
		if !strings.Contains(lane, want) {
			t.Errorf("project-lane.js no longer has %s; revisit crw-run, task-packet.md, script.md and journal.md", want)
		}
	}
	if strings.Contains(lane, "opts.sub") {
		t.Errorf("project-lane.js checks a sub-task alone (opts.sub); revisit crw-run, script.md and journal.md")
	}

	t.Run("fixture: a section without the sub-task loop is reported", func(t *testing.T) {
		good := "## Run a milestone with a flow run\n\n" +
			"A node the architect sized `split` with sub-tasks is released to one executor. Its sub-tasks run in their order on the node's one executor thread: the build does the first, each next one is a `subtask:<node>:<tier>:s<k>` turn of the same thread. " +
			"A sub-task before the last is committed as a checkpoint against its own regions, with no unit check, no verification and no land. " +
			"The last goes through the node's commit, verification, the check of the node's whole change against every criterion of the node, on which the acceptance rests, and land, so the node is judged once. A sub-task that reported failed is named in the next packet and judged with the node. " +
			"A fresh thread starts at the node's last commit, and its packet at the first sub-task not done. A split with no sub-tasks still goes up as `size_split`. " +
			"This thread sends no task packet for a sub-task.\n"
		if p := splitRunProblems(sectionOf(good, "Run a milestone with a flow run")); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, mutate := range map[string]func(string) string{
			"no executor": func(s string) string { return strings.Replace(s, "is released to one executor", "goes up", 1) },
			"no thread": func(s string) string {
				return strings.Replace(s, "on the node's one executor thread", "on executors", 1)
			},
			"no label": func(s string) string { return strings.Replace(s, "`subtask:<node>:<tier>:s<k>`", "next", 1) },
			"verified": func(s string) string {
				return strings.Replace(s, "with no unit check, no verification and no land", "verified and landed", 1)
			},
			"judged often": func(s string) string { return strings.Replace(s, "judged once", "judged each time", 1) },
			"sub-task check": func(s string) string {
				return strings.Replace(s, "is committed as a checkpoint against its own regions, with no unit check,",
					"is unit-checked against that sub-task alone and then committed as a checkpoint against its own regions, with", 1)
			},
			"checked": func(s string) string {
				return strings.Replace(s, "with no unit check, no verification", "with no verification", 1)
			},
			"no whole check": func(s string) string {
				return strings.Replace(s, "the check of the node's whole change against every criterion of the node, on which the acceptance rests, ", "", 1)
			},
			"failed": func(s string) string { return strings.Replace(s, "named in the next packet and ", "", 1) },
			"restart": func(s string) string {
				return strings.Replace(s, "at the first sub-task not done", "at the first sub-task", 1)
			},
			"no legacy": func(s string) string {
				return strings.Replace(s, "A split with no sub-tasks still goes up as `size_split`. ", "", 1)
			},
			"packet": func(s string) string { return strings.Replace(s, "sends no task packet", "sends a task packet", 1) },
			"old split": func(s string) string {
				return s + "Executors build it, so an issue the architect sizes as a split goes up as `size_split` and the child divides the plan's node.\n"
			},
		} {
			if p := splitRunProblems(sectionOf(mutate(good), "Run a milestone with a flow run")); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})

	t.Run("fixture: a document without the checkpoint commits is reported", func(t *testing.T) {
		good := "A split node makes one checkpoint `crw.commit` per sub-task before the last, each after a `subtask:<node>:<tier>:s<k>` turn, and is judged once, on the check of its whole change.\n"
		if p := splitDocProblems(good); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, bad := range map[string]string{
			"no checkpoint":  strings.Replace(good, "one checkpoint `crw.commit` per sub-task", "one commit", 1),
			"judged often":   strings.Replace(good, "is judged once", "is judged after each", 1),
			"no label":       strings.Replace(good, ", each after a `subtask:<node>:<tier>:s<k>` turn", "", 1),
			"old release":    good + "A node sized for a child or a split is not released to an executor.\n",
			"no whole":       strings.Replace(good, ", on the check of its whole change", "", 1),
			"sub-task check": good + "The unit check of a sub-task judges it alone, with the later ones as not due yet; the row records the `sub`.\n",
		} {
			if p := splitDocProblems(bad); len(p) == 0 {
				t.Errorf("%s: the changed text is accepted", label)
			}
		}
	})

	t.Run("fixture: a packet introduction without the sub-task turns is reported", func(t *testing.T) {
		good := "A node sized `split` with sub-tasks runs them on its one executor thread, each next one a turn the run sends (`subtask:<node>:<tier>:s<k>`), and the thread sends no packet for any of them.\n"
		if p := splitPacketProblems(good); len(p) > 0 {
			t.Fatalf("the good text is reported: %v", p)
		}
		for label, bad := range map[string]string{
			"no thread": strings.Replace(good, "on its one executor thread", "on executors", 1),
			"no label":  strings.Replace(good, " (`subtask:<node>:<tier>:s<k>`)", "", 1),
			"a packet":  strings.Replace(good, "sends no packet", "sends a packet", 1),
		} {
			if p := splitPacketProblems(bad); len(p) == 0 {
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
		`(?i)never divide an issue because a number crossed its line`,
		`(?i)the concept boundary decides`,
		"`over_line_accepted`", `(?i)bundle reason`,
		"`size_split`", "`region_order_missing`",
		// a run starts no `child` session (CRW-1415): a node that reaches it as `child` is escalated, and the planner splits instead
		"`child_retired`", `(?i)bundle split`, `(?i)starts? no \x60child\x60 session`,
		// CRW-1435 (S-1): a `split` node is divided into sub-tasks on one executor thread, a `bundle_split` node asks the parent for a
		// bundle split, and the compiler refuses `child`
		"`bundle_split`", "`bundle_split_request`", `(?i)sub-tasks`, `(?i)one executor thread`, `(?i)compiler refuses the value`,
		`(?i)no model name and no effort`,
		fmt.Sprintf(`(?i)a name, at most %d issues`, compileMaxGroup(t)),
	}); len(missing) > 0 {
		t.Errorf("Set the size fields lacks: %s\n%s", strings.Join(missing, "\n  "), fields)
	}
	// the size fields send no node to a `child` session: no planner or architect is told to set `child`
	for _, retired := range []string{"(?i)gets `size_fit` `child`", "(?i)may set `child`", `(?i)coordinates an issue-lane run`, `(?i)a child is for an issue that needs a coordinator`, `(?i)or send it to a child`} {
		if m := regexp.MustCompile(retired).FindString(fields); m != "" {
			t.Errorf("Set the size fields still sends a node to the retired `child` session: %q", m)
		}
	}
	orchestration := normalized(sectionOf(plan, "Write the Orchestration section of the project"))
	if missing := wordingMissing(orchestration, []string{
		"`## Orchestration`", `(?i)parallel width`, `(?i)region map`, `(?i)integration target`, `(?i)expected review rounds`, `(?i)names no model and no effort`,
	}); len(missing) > 0 {
		t.Errorf("the Orchestration section lacks: %s\n%s", strings.Join(missing, "\n  "), orchestration)
	}
	add := normalized(sectionOf(readRepoText(t, "plugins/crw/skills/crw-add-issue/SKILL.md"), "Set the size fields"))
	if missing := wordingMissing(add, []string{
		"`size_fit`", "`size_reason`", "`issue_size`", "`group`", "`crw skill size-fit`", `(?i)no count alone divides the issue or makes it a \x60split\x60`,
		`\(\.\./crw-plan/SKILL\.md#set-the-size-fields\)`,
	}); len(missing) > 0 {
		t.Errorf("crw-add-issue, Set the size fields, lacks: %s\n%s", strings.Join(missing, "\n  "), add)
	}
	// CRW-1435 (S-1): a size reason sends no issue to a child; it keeps the issue one node of sub-tasks
	if m := regexp.MustCompile(`(?i)sends? it to a child|a split goes back to the caller`).FindString(add); m != "" {
		t.Errorf("crw-add-issue, Set the size fields, still says %q", m)
	}
	if missing := wordingMissing(add, []string{"`size_fit` `split`", `(?i)sub-tasks`, `(?i)bundle split`}); len(missing) > 0 {
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
	// warnings is written by `crw flow plan-input` (CRW-1379: the project key warning it found), never by the author of a handoff, so the
	// section does not have to name it
	unset := []string{"request_id", "coordinator_epoch", "kind", "criteria_set_digest", "warnings"}
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

// The project key of a plan is the scope_key of the child's project binding, which is OWNER:NAME:N for a milestone plan (CRW-1379,
// the documents of the plan-input change): crw-plan and flow.md no longer call it the project's UUID, the order edge limit counts the
// ".N" suffix a re-plan adds, and the key warnings of plan-input are said to reach a pipe through the project's warnings member.
func TestPlanKeyDocsNameTheMilestoneKey(t *testing.T) {
	plan := normalized(readRepoText(t, planSkillFile))
	flow := normalized(readRepoText(t, "plugins/crw/skills/crw-canon/references/flow.md"))
	for name, c := range map[string]struct{ text, want, notWant string }{
		"crw-plan names the key form":     {plan, "`OWNER:NAME:N` for a milestone plan (`crw flow plan-input` prints it)", "the project's UUID"},
		"flow.md start fact names it":     {flow, "(`OWNER:NAME:N` for a milestone plan), so the run's first claim", "(the project's UUID)"},
		"flow.md counts the re-plan edge": {flow, "`e:FROM:TO.2` after a re-plan, so both keys fit in 123 characters together", "fit in 125 characters"},
		"flow.md says a pipe records it":  {flow, "are also the `warnings` member of the project in the bare input, which `crw flow compile` carries into the plan's warnings, so a pipe records them", ""},
	} {
		if !strings.Contains(c.text, c.want) {
			t.Errorf("%s: the text lacks %q", name, c.want)
		}
		if c.notWant != "" && strings.Contains(c.text, c.notWant) {
			t.Errorf("%s: the text still says %q", name, c.notWant)
		}
	}
}

// A task packet belongs to a `child` session of a plan with no run. A flow run starts no `child` session (CRW-1415): a node sized for one goes up
// as a `size_split` escalation with detail `child_retired` and the repository session asks for a bundle split; the nodes a run releases get no packet from
// the thread and start no PABCD state machine.
func taskPacketScopeProblems(intro string) []string {
	flat := normalized(intro)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)packets? (below )?(is|are) for an? \x60child\x60 session of a plan with no run`,
		`(?i)starts? no \x60child\x60 session`,
		"`size_split`",
		"`child_retired`",
		`(?i)bundle split`,
		`(?i)(sends|gets)( it)? no (task )?packet`,
		`(?i)no PABCD state machine`,
		`flow\.md#who-owns-control-in-a-flow-managed-run`,
	}) {
		problems = append(problems, "the introduction lacks "+p)
	}
	if m := regexp.MustCompile("(?i)`issue-lane`|issue-lane workflow|runs the issue lane").FindString(flat); m != "" {
		problems = append(problems, "the introduction sends a node a run releases to the retired issue lane ("+m+")")
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
	good := "The packets are for a `child` session of a plan with no run. A flow run starts no `child` session: a node sized for one goes up as a `size_split` escalation with detail `child_retired` and the repository session asks for a bundle split. A node a run releases gets no packet from the thread and starts no PABCD state machine ([control](../../crw-canon/references/flow.md#who-owns-control-in-a-flow-managed-run))."
	if p := taskPacketScopeProblems(good); len(p) > 0 {
		t.Fatalf("the good text is reported: %v", p)
	}
	for label, bad := range map[string]string{
		"no packet note":   strings.Replace(good, "gets no packet from the thread and ", "", 1),
		"no child_retired": strings.Replace(good, " with detail `child_retired`", "", 1),
		"no bundle split":  strings.Replace(good, " and the repository session asks for a bundle split", "", 1),
		"issue lane":       good + " The child of an oversized issue that a flow run releases runs the `issue-lane` workflow.",
	} {
		if p := taskPacketScopeProblems(bad); len(p) == 0 {
			t.Errorf("%s: the changed text is accepted", label)
		}
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
		"`crw flow reports --project PROJECT --unread`",
		`(?i)at the start of every turn`,
		// CRW-1390: a report arrives as a turn the relay daemon opened (CRW-1380), and the list is where one whose delivery is
		// pending or held is not missed
		`(?i)a turn the relay daemon opened in this thread`,
		`(?i)is not missed`,
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
		good := "## Read the milestone reports\n\nEach report arrives as a turn the relay daemon opened in this thread. Run `crw flow reports --project PROJECT --unread` at the start of every turn, so a report whose delivery is held is not missed. " +
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
	for _, sub := range []string{"start", "show", "watch", "reports", "report-ack", "report-up", "compile", "resume", "answer"} {
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
