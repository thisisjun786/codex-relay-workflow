package scenario

import (
	"errors"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// hookRelay installs the hook and the reporter over the supervisor channel, as the binary does after CRW-1380: the reports and the
// run's notices are staged by report.RelaySender; the fallback (send-parent) is the scenario's sender.
func (l *Lab) hookRelay(t *testing.T, fallback *scenarioSender) {
	t.Helper()
	rows := report.JournalAppender{Journal: l.journal, RunID: l.RunID, Store: l.Store}
	sender := report.RelaySender{Store: l.Store, Fallback: fallback}
	w := &report.Writer{Store: l.Store, Rows: rows, Resolver: report.StoreResolver{Store: l.Store}, Sender: sender, ArtifactDir: t.TempDir()}
	l.Reporter = report.Reporter{W: w, Run: l.RunID}
	l.Upward = &report.RunHook{Store: l.Store, Sched: &dagsched.Scheduler{Store: l.Store}, Journal: l.journal, Rows: rows, Sender: sender, Run: l.RunID,
		Reports: w, Blocked: flowcli.RunBlocked}
	l.newLane()
}

// blockedMessages are the flow_project_blocked messages the channel holds: their recipients and states.
func blockedMessages(t *testing.T, l *Lab) (recipients, states []string) {
	t.Helper()
	rows, err := l.Store.All(t.Context(), "SELECT recipient_task_id, state FROM supervisor_messages WHERE obligation_kind = ?", supervisor.KindFlowProjectBlocked)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		recipients, states = append(recipients, r.Text("recipient_task_id")), append(states, r.Text("state"))
	}
	return recipients, states
}

// CRW-1320 on the new path (CRW-1380): the run whose every release fails reports project_blocked through the supervisor channel - one
// flow_project_blocked message staged for the level above the project, one report row and one queued delivery row, nothing through
// send-parent for the report - and a resume that ends the same way stages nothing more.
func TestCRW1320_BlockedRunReportsOnceThroughTheSupervisorChannel(t *testing.T) {
	l := NewLab(t, apart()...)
	fallback := l.wireUpward(t, true)
	l.hookRelay(t, fallback)
	l.Relay.ReleaseErr = func(node string) error {
		return errors.New("the relay refused the release of " + node + ": stale_coordinator_epoch")
	}
	if got := mustRunIncomplete(t, l); got.State != "incomplete" {
		t.Fatalf("value %+v", got)
	}
	check := func(when string) {
		t.Helper()
		recipients, states := blockedMessages(t, l)
		sent, rows := projectBlocked(l, fallback)
		if len(recipients) != 1 || recipients[0] != upLead || states[0] != "queued" || len(sent) != 0 || len(rows) != 1 {
			t.Fatalf("%s: messages to %v (%v), sent through send-parent %d, report rows %d", when, recipients, states, len(sent), len(rows))
		}
		deliveries := 0
		for _, r := range l.FlowRows(report.KindDelivery) {
			if str(r.Data, "report_id") == str(rows[0].Data, "report_id") {
				if str(r.Data, "result") != report.ResultQueued {
					t.Fatalf("%s: a staging wrote %q", when, str(r.Data, "result"))
				}
				deliveries++
			}
		}
		if deliveries != 1 {
			t.Fatalf("%s: %d delivery rows, want the one queued", when, deliveries)
		}
	}
	check("the run")
	l.NewProcess("proc-2")
	l.hookRelay(t, fallback)
	if res := l.Resume(); res.Status != engine.RunCompleted {
		t.Fatalf("resume: %s %s %s", res.Status, res.ErrorKind, res.Error)
	}
	check("after the resume")
}
