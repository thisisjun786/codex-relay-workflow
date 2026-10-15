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
// end and no completion notice instead of a run that finishes held; the answer section still refuses a run a process holds;
// reporting.md names the milestone state (state=, reason=) in the completion notice; journal.md names the run paused row.
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
	answer := docSection(t, ref, "### crw flow answer")
	if !strings.Contains(answer, "a run a process holds is refused (`crw flow pause` first)") {
		t.Errorf("the answer section of flow.md no longer refuses a run a process holds:\n%s", answer)
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
