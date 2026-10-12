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
