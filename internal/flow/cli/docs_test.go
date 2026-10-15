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
