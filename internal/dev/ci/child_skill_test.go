//go:build dev

package ci

import (
	"regexp"
	"strings"
	"testing"
)

// CRW-1353 (CRW-1316 S4, design-1316 sections 13 and 18): the child's skill and the documents it reads describe the session
// call path and the way a child works. crw-run's default path is a flow run whose steps the child spawns as role subagents
// (`crw flow next`, spawn_agent, `crw flow dispatched`), and a `child` session of a plan with no run is the exception; the child splits work
// and spawns subagents broadly, follows the CXC sequence (architect, the critic's design check, executors in parallel, a reviewer of another
// family, a fix round, verification, landing, an explorer when needed), chooses the DAG or another way while CRW enforces the
// order, the gates and the records, and is capped by agents.max_threads; ambiguity goes up as a decision request, never as an
// interview; the PR bundle is only named. The canon's flow page, the spawn policy, the dispatch surfaces and the flow-bound
// dispatch card say the same.

// childSessionLoopProblems judges crw-run's flow run section for the step exchange of design section 3.2.
func childSessionLoopProblems(section string) []string {
	flat := normalized(section)
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		"`crw flow next ",
		"`spawn_agent`",
		"`fork_context` false",
		"`crw flow dispatched ",
		"`send_input`",
		"`close_agent`",
		"`wait_agent`",
		"`agent thread limit reached`",
		`(?i)never copies a subagent's answer`,
		`(?i)the run reads (it|the answer) from the host`,
		`(?i)resumes? (the run|it) (itself )?when no process holds it`,
		`(?i)preview`,
		`(?i)epoch`,
		"`crw flow report-up`",
	}) {
		problems = append(problems, "the flow run section lacks "+p)
	}
	return problems
}

// childWorkingWayProblems judges crw-run's section on how a child works (design sections 18.2 to 18.5).
func childWorkingWayProblems(section string) []string {
	flat := normalized(section)
	if flat == "" {
		return []string{"crw-run has no section \"How the child works\""}
	}
	var problems []string
	for _, p := range wordingMissing(flat, []string{
		`(?i)splits? the work`,
		`(?i)in parallel`,
		`(?i)decides? the next step (from|by) (the|their) results?`,
		`(?i)architect[^.]*critic's design check[^.]*executors? in parallel[^.]*reviewer of another (model )?family[^.]*fix round[^.]*verification[^.]*land`,
		`(?i)explorer[^.]*(when|whenever) (it is )?needed`,
		`(?i)the child (decides|chooses) whether (to use )?the DAG`,
		`(?i)CRW enforces the order, the gates and the records`,
		"`agents.max_threads`",
		`(?i)decision request`,
		`(?i)never (opens )?an interview`,
		`(?i)pull request`,
		`(?i)lands? (each|one) node`,
		"CRW-1355",
		`(?i)independence_refused`,
		"verification-record/1",
	}) {
		problems = append(problems, "the section lacks "+p)
	}
	return problems
}

func TestChildSkillRunsTheSessionLoop(t *testing.T) {
	text := readRepoText(t, runSkillFile)
	if p := childSessionLoopProblems(sectionOf(text, "Run a milestone with a flow run")); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	t.Run("fixture: a section without the step exchange is reported", func(t *testing.T) {
		if p := childSessionLoopProblems("## Run a milestone with a flow run\n\nStart it with `crw flow start`.\n"); len(p) < 10 {
			t.Errorf("only %d problems: %v", len(p), p)
		}
	})
}

func TestChildSkillSaysHowTheChildWorks(t *testing.T) {
	text := readRepoText(t, runSkillFile)
	if p := childWorkingWayProblems(sectionOf(text, "How the child works")); len(p) > 0 {
		t.Errorf("%s:\n  %s", runSkillFile, strings.Join(p, "\n  "))
	}
	t.Run("fixture: an interview route is reported", func(t *testing.T) {
		if p := childWorkingWayProblems("## How the child works\n\nOpen an interview when a question comes up.\n"); len(p) < 10 {
			t.Errorf("only %d problems: %v", len(p), p)
		}
	})
}

// TestChildSkillDefaultPathIsTheFlowRun: the opening of crw-run makes the flow run with role subagents the default path, says that a
// node sized for a `child` session starts no `child` session (the project lane escalates it as size_split with detail child_retired and the
// parent asks for a bundle split, CRW-1415), keeps the `child` session of a plan with no run the exception, and the rule against a subagent
// standing in for a `child` session stays on that path only.
func TestChildSkillDefaultPathIsTheFlowRun(t *testing.T) {
	text := readRepoText(t, runSkillFile)
	opening := normalized(text[:strings.Index(text, "\n## ")])
	for _, p := range wordingMissing(opening, []string{
		`(?i)default path[^.]*flow run[^.]*(role )?subagents?`,
		`(?i)starts? no \x60child\x60 session`,
		"`size_split`",
		"`child_retired`",
		`(?i)bundle split`,
		`(?i)exception`,
	}) {
		t.Errorf("the opening of crw-run lacks %s", p)
	}
	if m := regexp.MustCompile(`(?i)SP-CHILD-1|releases? (that|the) \x60child\x60 session`).FindString(opening); m != "" {
		t.Errorf("the opening of crw-run says the run starts a `child` session (%s), a path the project lane retired", m)
	}
	issue := normalized(sectionOf(text, "Independent implementation tasks"))
	for _, p := range wordingMissing(issue, []string{
		`(?i)the exception path`,
		`(?i)a step the run records`,
		`(?i)for a plan with no flow run`,
	}) {
		t.Errorf("the `child` session section lacks %s", p)
	}
	if m := regexp.MustCompile(`SP-CHILD-[12]`).FindString(issue); m != "" {
		t.Errorf("the `child` session section names the id %s of the retired path", m)
	}
}

// TestFlowPageNamesTheStepExchange: the canon's flow page has the two commands of the step exchange in its command table and the
// exchange in its ownership table.
func TestFlowPageNamesTheStepExchange(t *testing.T) {
	text := readRepoText(t, flowRef)
	commands := normalized(sectionOf(text, "The crw flow command table"))
	for _, want := range []string{"`crw flow next RUN", "`crw flow dispatched RUN CALL"} {
		if !strings.Contains(commands, want) {
			t.Errorf("the command table lacks %q", want)
		}
	}
	owner := normalized(ownershipSection(text))
	if !regexp.MustCompile(`(?i)\| the step exchange of a session run \| the run publishes[^|]*the child spawns`).MatchString(owner) {
		t.Errorf("the ownership table has no row for the step exchange:\n%s", owner)
	}
}

// TestSpawnPolicyWakeAndHelperRules: SP-INL-4 wakes the child for a step of its run and never for one issue's event, and SP-NAT-1
// is the rule of a native spawn that is no step (an engine call's explorer, or a helper of the child), in the page and in rules.go.
func TestSpawnPolicyWakeAndHelperRules(t *testing.T) {
	page := readRepoText(t, spawnPolicyRef)
	rules := readRepoText(t, spawnRulesGo)
	row := func(id string) string {
		for _, line := range lines(page) {
			if strings.HasPrefix(strings.TrimSpace(line), "| `"+id+"` |") {
				return line
			}
		}
		return ""
	}
	comment := func(name string) string {
		m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*"SP-[A-Z0-9-]+"\s*//(.*)$`).FindStringSubmatch(rules)
		if m == nil {
			return ""
		}
		return m[1]
	}
	for where, text := range map[string]string{"the SP-INL-4 row": row("SP-INL-4"), "RuleInl4": comment("RuleInl4")} {
		if !regexp.MustCompile(`(?i)never woken for one issue's event`).MatchString(text) || !regexp.MustCompile(`(?i)woken for a step of its run`).MatchString(text) {
			t.Errorf("%s does not wake the child for a run step and never for one issue's event: %q", where, text)
		}
	}
	for where, text := range map[string]string{"the SP-NAT-1 row": row("SP-NAT-1"), "RuleNat1": comment("RuleNat1")} {
		if !regexp.MustCompile(`(?i)no step`).MatchString(text) || !regexp.MustCompile(`(?i)helper of the child`).MatchString(text) {
			t.Errorf("%s is not the rule of a native spawn that is no step: %q", where, text)
		}
	}
	if !strings.Contains(normalized(row("SP-NAT-1")), "`unrecorded_spawn`") {
		t.Errorf("the SP-NAT-1 row does not say a helper of the child is recorded `unrecorded_spawn`")
	}
}

// TestDispatchSurfacesNameTheChildsStepSpawns: the dispatch surfaces of crw-pabcd no longer keep the child's thread to read-only
// and review helpers: it spawns the run's role subagents for the steps it is given.
func TestDispatchSurfacesNameTheChildsStepSpawns(t *testing.T) {
	text := normalized(readRepoText(t, "plugins/crw/skills/crw-pabcd/references/dispatch-surfaces.md"))
	if strings.Contains(text, "uses read-only and review helpers only") {
		t.Error("dispatch-surfaces.md still keeps the child's thread to read-only and review helpers")
	}
	if strings.Contains(text, "an engine thread of its own, not a native helper of any task") {
		t.Error("dispatch-surfaces.md still says the run starts the executor as an engine thread of its own")
	}
	if !regexp.MustCompile("(?i)spawns? [^.]*role subagents?[^.]*`crw flow next`").MatchString(text) {
		t.Error("dispatch-surfaces.md does not say the child spawns the run's role subagents for the steps `crw flow next` gives")
	}
}

// TestDispatchSurfacesNameTheAssignedReceiptTree: the bounded checkout worker of crw-pabcd writes its receipt under the tree the
// dispatch assigned (CRW-1115's `CRW-WORKTREE:` assignment, CRW-1482's flow executor file), not under the coordinator's native cwd;
// only a dispatch with no assignment keeps the native cwd's `.crw/evidence`.
func TestDispatchSurfacesNameTheAssignedReceiptTree(t *testing.T) {
	var worker string
	for _, para := range strings.Split(strings.ReplaceAll(readRepoText(t, "plugins/crw/skills/crw-pabcd/references/dispatch-surfaces.md"), "\r\n", "\n"), "\n\n") {
		if strings.Contains(para, "**bounded checkout worker**") {
			worker = normalized(para)
			break
		}
	}
	if worker == "" {
		t.Fatal("dispatch-surfaces.md has no bounded checkout worker paragraph")
	}
	for _, stale := range []string{"reads `.crw/evidence` under the coordinator's native cwd", "write its receipt under the native cwd's"} {
		if strings.Contains(worker, stale) {
			t.Errorf("the bounded checkout worker paragraph still says the gate %q", stale)
		}
	}
	for _, want := range []string{"`CRW-WORKTREE: <path>`", "`<P>/.crw/evidence/<id>.md`"} {
		if !strings.Contains(worker, want) {
			t.Errorf("the bounded checkout worker paragraph does not name %s", want)
		}
	}
	fallback := false
	for _, sentence := range strings.Split(worker, ". ") {
		if regexp.MustCompile(`(?i)\b(no|without an?) (evidence )?assignment`).MatchString(sentence) && strings.Contains(sentence, "native cwd") && strings.Contains(sentence, "`.crw/evidence`") {
			fallback = true
		}
	}
	if !fallback {
		t.Errorf("the bounded checkout worker paragraph does not say a dispatch with no assignment writes under the native cwd's `.crw/evidence`:\n%s", worker)
	}
}

// TestScriptPageKeepsTheAppServerBackendForTests: script.md says the App Server backend is the backend of tests and shadow runs,
// and that a started run runs as session calls.
func TestScriptPageKeepsTheAppServerBackendForTests(t *testing.T) {
	section := normalized(sectionOf(readRepoText(t, "docs/flow/script.md"), "The App Server backend"))
	if !regexp.MustCompile("(?i)`crw flow start` runs a run as session calls[^.]*by default").MatchString(section) || !regexp.MustCompile(`(?i)tests and shadow runs`).MatchString(section) {
		t.Errorf("the App Server backend section does not say it is the backend of tests and shadow runs:\n%.400s", section)
	}
}
