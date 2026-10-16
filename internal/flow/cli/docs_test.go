package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	schedulerDoc = "../../../docs/relay/dag-scheduler.md"
	flowRef      = "../../../plugins/crw/skills/crw-canon/references/flow.md"
)

// tableRows are the rows of the markdown table that follows the line `marker`, with the header and the separator left out.
func tableRows(t *testing.T, text, marker string) []string {
	t.Helper()
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no %q in the document", marker)
	}
	var rows []string
	for _, line := range strings.Split(text[i+len(marker):], "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" && len(rows) == 0 {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		rows = append(rows, line)
	}
	if len(rows) < 3 {
		t.Fatalf("the table after %q has %d lines", marker, len(rows))
	}
	return rows[2:]
}

// TestSchedulerDocRestartRowNamesRunCommands: the restart table of dag-scheduler.md has the row of a flow run process that died, the
// row names `crw flow <sub>` commands, every one of them is in the command table of flow.md, and the resident-process sentences are
// gone.
func TestSchedulerDocRestartRowNamesRunCommands(t *testing.T) {
	raw, err := os.ReadFile(schedulerDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	var row string
	for _, r := range tableRows(t, doc, "What each kind of restart recovers:") {
		if strings.HasPrefix(r, "| a flow run process that died |") {
			row = r
		}
	}
	if row == "" {
		t.Fatal("the restart table has no row for a flow run process that died")
	}
	named := regexp.MustCompile("`?crw flow ([a-z][a-z-]*)").FindAllStringSubmatch(row, -1)
	if len(named) == 0 {
		t.Fatalf("the row names no crw flow command: %s", row)
	}
	ref, err := os.ReadFile(flowRef)
	if err != nil {
		t.Fatal(err)
	}
	table := tableRows(t, string(ref), "## The crw flow command table")
	listed := func(sub string) bool {
		for _, r := range table {
			if strings.HasPrefix(r, "| `crw flow "+sub+" ") || strings.HasPrefix(r, "| `crw flow "+sub+"`") || strings.HasPrefix(r, "| `crw flow "+sub+",") {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	for _, m := range named {
		seen[m[1]] = true
		if !listed(m[1]) {
			t.Errorf("the restart row names `crw flow %s`, which the command table of flow.md does not list", m[1])
		}
	}
	for _, want := range []string{"resume", "recover"} {
		if !seen[want] {
			t.Errorf("the restart row does not name `crw flow %s`", want)
		}
	}
	for _, want := range []string{"run lock", "journal", "replaying the same request"} {
		if !strings.Contains(row, want) {
			t.Errorf("the restart row does not say %q", want)
		}
	}
	if strings.Contains(doc, "Nothing is resident") {
		t.Error("dag-scheduler.md still says \"Nothing is resident\"")
	}
	lower := strings.ToLower(doc)
	for _, want := range []string{"the scheduler stays a library; a flow run is a process that lives for one run and loses nothing when it dies", "the scheduler stays a library, and a flow run is a process that lives for one run and loses nothing when it dies"} {
		if !strings.Contains(lower, want) {
			t.Errorf("dag-scheduler.md does not carry %q", want)
		}
	}
	for _, value := range []string{"adopt", "adopt_needed", "reconcile", "needs_operator", "none"} {
		if !strings.Contains(doc, "`"+value+"`") {
			t.Errorf("the Resume table lost the value %s", value)
		}
	}
	if !strings.Contains(doc, "mean the same when the process that restarts is a flow run process") {
		t.Error("dag-scheduler.md does not say the Resume values mean the same after a run process restarted")
	}
	for _, r := range tableRows(t, doc, "What each kind of restart recovers:") {
		if strings.Contains(strings.ToLower(r), "daemon") || strings.Contains(strings.ToLower(r), "resident") {
			t.Errorf("a restart row presumes a resident process: %s", r)
		}
	}
}

// docSection is the text of the markdown section that starts at the heading line `heading`, up to the next heading of the same or a
// higher level.
func docSection(t *testing.T, text, heading string) string {
	t.Helper()
	i := strings.Index(text, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("no heading %q in the document", heading)
	}
	level := strings.Index(heading, " ")
	rest := text[i+len(heading)+2:]
	off := 0
	for _, line := range strings.SplitAfter(rest, "\n") {
		if n := strings.Index(line, " "); n > 0 && n <= level && strings.Trim(line[:n], "#") == "" {
			return rest[:off]
		}
		off += len(line)
	}
	return rest
}

// TestFlowRefDescribesThePausedEnd: the docs follow the paused end of CRW-1487. The pause section of flow.md names the run's paused
// end and no completion notice instead of a run that finishes held; reporting.md names the milestone state (state=, reason=) in the completion notice; journal.md names the run paused row.
func TestFlowRefDescribesThePausedEnd(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	ref := read(flowRef)
	pause := docSection(t, ref, "### crw flow pause")
	if strings.Contains(pause, "finishes `held`") {
		t.Errorf("the pause section of flow.md still says the run finishes held:\n%s", pause)
	}
	for _, want := range []string{"`paused`", "`run/paused`", "no completion notice", "crw flow resume"} {
		if !strings.Contains(pause, want) {
			t.Errorf("the pause section of flow.md does not say %q:\n%s", want, pause)
		}
	}
	reporting := read("../../../docs/flow/reporting.md")
	for _, want := range []string{"state=", "reason=", "lanes="} {
		if !strings.Contains(reporting, want) {
			t.Errorf("reporting.md does not name %q in the completion notice", want)
		}
	}
	journal := read("../../../docs/flow/journal.md")
	if !strings.Contains(journal, "`run/paused`") {
		t.Error("journal.md does not name the run/paused row")
	}
}

// TestFlowDocsNameEvidencePlaced (CRW-1496): the docs name the evidence_placed call row. script.md lists it among the rows a session
// call journals and in the session row table, and its bind paragraph says the bind writes it before a copy under the host cwd;
// journal.md lists it among the intents committed before Append returns.
func TestFlowDocsNameEvidencePlaced(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	script := read("../../../docs/flow/script.md")
	if journal := docSection(t, script, "## Journal"); !strings.Contains(journal, "`evidence_placed`") {
		t.Error("the Journal section of script.md does not name the evidence_placed row of a session call")
	}
	table := ""
	if i := strings.Index(script, "| Row | In a session call |"); i >= 0 {
		table = script[i:]
		if end := strings.Index(table, "\n\n"); end >= 0 {
			table = table[:end]
		}
	}
	if !strings.Contains(table, "\n| `evidence_placed` |") {
		t.Error("the session row table of script.md has no evidence_placed row")
	}
	bind := ""
	for _, line := range strings.Split(script, "\n- ") {
		if strings.HasPrefix(line, "A bind writes `thread_started`") {
			bind = line
		}
	}
	if !strings.Contains(bind, "`evidence_placed`") {
		t.Errorf("the bind paragraph of script.md does not name evidence_placed:\n%s", bind)
	}
	intents := ""
	for _, line := range strings.Split(read("../../../docs/flow/journal.md"), "\n- ") {
		if strings.HasPrefix(line, "A dispatch intent (") {
			intents = line
		}
	}
	if !strings.Contains(intents, "`evidence_placed`") {
		t.Errorf("the intent list of journal.md does not name evidence_placed:\n%s", intents)
	}
}

// TestOutcomeDocsStateThePrecedence: the three docs that tell which state a finished run reports (reporting.md, script.md and flow.md)
// state the rule outcomeOf applies: the milestone member that is an object without an id and with a non-empty string state is the
// wrapped result; a milestone member that carries an id is not read as one; otherwise the top-level string state is the outcome.
func TestOutcomeDocsStateThePrecedence(t *testing.T) {
	// The phrase that says a milestone member carrying an id, or without a non-empty string state, is not read as the wrapper.
	// Each doc words it differently, so each names its own sentence.
	notRead := map[string]string{
		"../../../docs/flow/reporting.md": "and is not read; a `milestone` member without",
		"../../../docs/flow/script.md":    "and is not read as one. The outcome state",
		flowRef:                           "is not read as one, and otherwise",
	}
	for _, path := range []string{
		"../../../docs/flow/reporting.md",
		"../../../docs/flow/script.md",
		flowRef,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{
			"without an `id`",
			"non-empty string `state`",
			"wrapped result",
			"carries an `id`",
			"otherwise the top-level string `state`",
			"non-empty string has no outcome",
			notRead[path],
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not state the outcome precedence: no %q", path, want)
			}
		}
	}
}

// TestDocsDescribeTheWriterHoldAndTheCancelDeadMember: the docs state the judgments of CRW-1495 that have no code. journal.md says
// when a cancel's finished row carries `dead: true` and that readers do not read it, and that a binary before CRW-1487 reads a
// paused end as dead (a rollback); script.md says the top-level string `state` of a completed script is its outcome whatever it means
// to the script; flow.md says a writer's short hold of the run lock is not a process of the run (ps), that cancel waits up to 2 s for
// it (cancel), and that every resume refuses a cancelled run (resume). The pins guard that the sentences exist; the behaviour is
// pinned by the tests of the code (writer_hold_test.go, resume_refusal_test.go).
func TestDocsDescribeTheWriterHoldAndTheCancelDeadMember(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	journal := read("../../../docs/flow/journal.md")
	for _, want := range []string{"`dead: true`", "do not read the `dead` member", "before CRW-1487"} {
		if !strings.Contains(journal, want) {
			t.Errorf("journal.md does not say %q", want)
		}
	}
	script := read("../../../docs/flow/script.md")
	if !strings.Contains(script, "whatever that state means to the script") {
		t.Error("script.md does not say the top-level state is the outcome whatever it means to the script")
	}
	ref := read(flowRef)
	sections := map[string][]string{
		"### crw flow ps":     {"is not a process of the run"},
		"### crw flow pause":  {"signals nothing"},
		"### crw flow cancel": {"waits up to 2 s"},
		"### crw flow resume": {"A cancelled run is over", "refuses it"},
	}
	for heading, wants := range sections {
		text := docSection(t, ref, heading)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("the section %q of flow.md does not say %q:\n%s", heading, want, text)
			}
		}
	}
	for _, r := range tableRows(t, ref, "## The crw flow command table") {
		if strings.HasPrefix(r, "| `crw flow resume RUN [SCRIPT] [--detach]` |") && !strings.Contains(r, "cancelled run") {
			t.Errorf("the resume row of the command table does not refuse a cancelled run: %s", r)
		}
	}
}

// TestFlowRefDescribesTheAnswerDrain: the docs follow the answer drain of CRW-1492. The answer row of flow.md's command table takes
// `--wait S`; the answer section says a run a process holds is asked to stop at its next boundary, that `--wait` bounds the wait for
// its lock, that `run_draining` says to run the same answer again, that it is a stop asked (`drain_asked`) which a process an older
// crw runs drops, that a decision withdrawn (decision-withdraw) while the answer waited is refused, and that only the answer that
// appends the row resumes the run; the next section keeps the owner's loop while the run drains; the crw-run skill sends the owner
// back to that loop on `run_draining`, and to a pause when an older crw never stops; script.md no longer says an answer holds the run
// "between two processes". The pins guard that the sentences exist; the behaviour is pinned by the tests of the code (answer_test.go,
// next_drain_test.go, process_test.go).
func TestFlowRefDescribesTheAnswerDrain(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	ref := read(flowRef)
	answer := docSection(t, ref, "### crw flow answer")
	for _, want := range []string{"next boundary", "--wait", "run_draining", "only the answer that appends", "drain_asked", "older crw", "decision-withdraw"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the answer section of flow.md does not say %q:\n%s", want, answer)
		}
	}
	if strings.Contains(answer, "(`crw flow pause` first)") {
		t.Errorf("the answer section of flow.md still sends the caller to pause first:\n%s", answer)
	}
	row := ""
	for _, r := range tableRows(t, ref, "## The crw flow command table") {
		if strings.HasPrefix(r, "| `crw flow answer ") {
			row = r
		}
	}
	if !strings.Contains(row, "[--wait S]") || !strings.Contains(row, "next boundary") {
		t.Errorf("the answer row of the command table does not take --wait or drain a run a process holds: %s", row)
	}
	next := docSection(t, ref, "### crw flow next and crw flow dispatched")
	if !strings.Contains(next, "a step nobody takes holds its call and the run") {
		t.Errorf("the next section of flow.md does not keep the owner's loop while a run drains:\n%s", next)
	}
	skill := read("../../../plugins/crw/skills/crw-run/SKILL.md")
	for _, want := range []string{"run_draining", "a step nobody takes holds its call and the run", "older crw"} {
		if !strings.Contains(skill, want) {
			t.Errorf("the crw-run skill does not say %q", want)
		}
	}
	if script := read("../../../docs/flow/script.md"); strings.Contains(script, "between two processes") {
		t.Error("script.md still says an answer holds the run between two processes")
	}
}

// TestDocsNameTheBundlePullRequestSurface (CRW-1440, CRW-1451, CRW-1457): the docs that tell how a milestone lands through one pull
// request (script.md and flow.md) name the surface a bundle uses: the command that makes the integration branch, the `pr_base` that
// marks a handoff as a bundle, the command that opens the pull request from a run, the op that asks the parent to merge it and the rule
// that it asks once per head. The run skill names the command that makes the branch.
func TestDocsNameTheBundlePullRequestSurface(t *testing.T) {
	for _, path := range []string{
		"../../../docs/flow/script.md",
		flowRef,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{
			"crw flow pr branch",
			"pr_base",
			"crw flow pr open --run",
			"crw.mergeRequest",
			"once per head",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not name the bundle pull request surface: no %q", path, want)
			}
		}
	}
	raw, err := os.ReadFile("../../../plugins/crw/skills/crw-run/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "crw flow pr branch") {
		t.Error("crw-run/SKILL.md does not name crw flow pr branch")
	}
}

// TestReportingDocNamesTheMergeRequestReport (CRW-1457): reporting.md names the merge request report the `crw.mergeRequest` op hands to
// the reporter: its kind (`merge_request`), the kind it goes through the supervisor channel under (`flow_merge_request`), the op, the
// rule that it asks once per head, and that the built-in `project-lane` does not send one. The milestone reports section says it, and the
// section of the supervisor channel lists the kind among the kinds of a report.
func TestReportingDocNamesTheMergeRequestReport(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/flow/reporting.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	reports := docSection(t, text, "## Milestone reports and project-level reports")
	for _, want := range []string{
		"`merge_request`",
		"`flow_merge_request`",
		"`crw.mergeRequest`",
		"once per head",
		"built-in `project-lane`",
	} {
		if !strings.Contains(reports, want) {
			t.Errorf("the milestone reports section of reporting.md does not name the merge request report: no %q", want)
		}
	}
	channel := docSection(t, text, "## Through the supervisor channel")
	if !strings.Contains(channel, "`flow_merge_request`") {
		t.Error("the supervisor channel section of reporting.md does not list `flow_merge_request` among the kinds of a report")
	}
}
