package dagsched

import (
	"os"
	"strings"
	"testing"
)

// CRW-283: the page, the skill and the command specs say the same seven commands, every state of an entry and every outcome a reconcile gives, so a command, state or outcome that is added or
// renamed in the code cannot go undescribed.

var summaryCommands = []string{"dag-summary-enqueue", "dag-summary-status", "dag-summary-claim", "dag-summary-reconcile", "dag-summary-complete", "dag-summary-fail", "dag-summary-retry"}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSummaryPageSkillAndSpecsNameTheSameCommands(t *testing.T) {
	page := readText(t, "../../../docs/relay/dag-outbox.md")
	skill := readText(t, "../../../plugins/crw/skills/crw-run/references/relay.md")
	specs := readText(t, "../argparse/specs.json")
	scheduler := readText(t, "../../../docs/relay/dag-scheduler.md")
	readme := readText(t, "../../../docs/relay/README.md")
	for _, command := range summaryCommands {
		for where, text := range map[string]string{"docs/relay/dag-outbox.md": page, "the crw-run skill (relay.md)": skill, "docs/relay/dag-scheduler.md": scheduler} {
			if !strings.Contains(text, command) {
				t.Errorf("%s does not name %s", where, command)
			}
		}
		if !strings.Contains(specs, "{\"name\":\""+command+"\"") {
			t.Errorf("argparse/specs.json has no spec for %s", command)
		}
	}
	for _, state := range []string{SummaryPending, SummaryClaimed, SummaryConfirmed, SummaryFailed, SummarySuperseded} {
		if !strings.Contains(page, "`"+state+"`") {
			t.Errorf("the page does not describe the state %s", state)
		}
	}
	for _, outcome := range []string{"already_written", "absent", "stale", "duplicate", "malformed"} {
		if !strings.Contains(page, "`"+outcome+"`") || !strings.Contains(skill, outcome) {
			t.Errorf("the page or the skill does not describe the outcome %s", outcome)
		}
	}
	for _, repair := range []string{"replace_container", "initialize", "manual"} {
		if !strings.Contains(page, "`"+repair+"`") || !strings.Contains(skill, repair) {
			t.Errorf("the page or the skill does not describe the repair %s", repair)
		}
	}
	for _, name := range []string{"dag_summary_outbox", "dag_summary_outbox_open", "dag_summary_outbox_order", "dag_summary_outbox_newest", "dag_summary_outbox_no_delete"} {
		if !strings.Contains(page, name) {
			t.Errorf("the page does not name the zone object %s", name)
		}
	}
	if !strings.Contains(readme, "dag-outbox.md") {
		t.Error("the relay README does not link the page")
	}
	if !strings.Contains(skill, "## The Linear summary of a DAG plan") || !strings.Contains(readText(t, "../../../plugins/crw/skills/crw-run/SKILL.md"), "references/relay.md#the-linear-summary-of-a-dag-plan") {
		t.Error("the skill's procedure or the link to it is missing")
	}
	// the relay holds no Linear credential: nothing of this change reads an environment variable or names a token or a client of Linear
	for _, file := range []string{"outbox.go", "outbox_block.go", "outbox_cli.go"} {
		src := readText(t, file)
		for _, banned := range []string{"os.Getenv", "net/http", "LINEAR_", "api.linear.app", "Authorization"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q: the relay holds no Linear credential and makes no Linear call", file, banned)
			}
		}
	}
}
