package scenario

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

// apart are two issue nodes that wait for nothing: both are released at once.
func apart() []NodeSpec {
	return []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
}

// showOf runs `crw flow show RUN` over the lab's store as a process of its own.
func showOf(t *testing.T, l *Lab) string {
	t.Helper()
	_ = l.Store.Close()
	defer func() { l.Store = l.openStore() }()
	var out, errOut bytes.Buffer
	if code := flowcli.RunWith(context.Background(), flowcli.Default(), []string{"show", l.RunID, "--state", l.Root, "--socket", l.Host.SocketPath}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("show: exit %d: %s", code, errOut.String())
	}
	return out.String()
}

// projectBlocked are the lead's notices and the report rows of kind project_blocked.
func projectBlocked(l *Lab, sender *scenarioSender) (sent []report.SupervisorSend, rows []engine.Event) {
	for _, m := range sender.Lead {
		if strings.Contains(m.Text, report.KindProjectBlocked) {
			sent = append(sent, m)
		}
	}
	for _, r := range l.FlowRows(report.KindReport) {
		if str(r.Data, "report_kind") == report.KindProjectBlocked {
			rows = append(rows, r)
		}
	}
	return sent, rows
}

// A milestone whose every release fails (the relay refuses it: the lanes are null and the script returns incomplete, so the engine
// finishes the run completed) is shown blocked, with the reason of each release, and the run reports project_blocked to the initiative
// lead once. A resume that ends the same way finds the report and sends nothing more.
func TestCRW1320_MilestoneWhoseReleasesAllFailIsBlockedAndReportsOnce(t *testing.T) {
	l := NewLab(t, apart()...)
	sender := l.wireUpward(t, true)
	l.Relay.ReleaseErr = func(node string) error {
		return errors.New("the relay refused the release of " + node + ": stale_coordinator_epoch")
	}
	got := mustRunIncomplete(t, l)
	if got.State != "incomplete" || len(l.Relay.Landed) != 0 {
		t.Fatalf("value %+v, landed %v", got, l.Relay.Landed)
	}
	for _, ln := range got.Lanes {
		if ln.State != "" {
			t.Fatalf("a lane came to %+v: every release fails", ln)
		}
	}
	d := flowcli.Detail(flowcli.RunRecord{RunID: l.RunID}, l.Events(), false, 0)
	if d.State != flowcli.StateBlocked || d.Status != engine.RunCompleted || len(d.ItemErrors) != 2 {
		t.Fatalf("detail: state %q status %q item errors %+v", d.State, d.Status, d.ItemErrors)
	}
	out := showOf(t, l)
	for _, want := range []string{"project-lane  blocked", "no node started", "item A failed:", "the relay refused the release of A: stale_coordinator_epoch", "item B failed:"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	sent, rows := projectBlocked(l, sender)
	if len(sent) != 1 || len(rows) != 1 || sent[0].Thread != upLead || str(rows[0].Data, "recipient_thread") != upLead {
		t.Fatalf("project_blocked: sent %+v rows %+v", sent, rows)
	}
	if !strings.Contains(sent[0].Text, "stale_coordinator_epoch") || !strings.Contains(sent[0].Text, "run="+l.RunID) {
		t.Errorf("the lead was told %q", sent[0].Text)
	}
	l.NewProcess("proc-2")
	l.hookUpward(t, sender)
	if res := l.Resume(); res.Status != engine.RunCompleted {
		t.Fatalf("resume: %s %s %s", res.Status, res.ErrorKind, res.Error)
	}
	if d := flowcli.Detail(flowcli.RunRecord{RunID: l.RunID}, l.Events(), false, 0); d.State != flowcli.StateBlocked || d.Processes != 2 {
		t.Fatalf("after the resume: state %q processes %d", d.State, d.Processes)
	}
	if sent, rows := projectBlocked(l, sender); len(sent) != 1 || len(rows) != 1 {
		t.Fatalf("after the resume: project_blocked sent %d, rows %d", len(sent), len(rows))
	}
}

// Nodes the scheduler holds back for capacity are a wait a resume asks again (CRW-1201), not a run that is blocked; a milestone where one
// release fails and the other node lands is a run where a node started. Both keep today's state, completed, and report no project_blocked.
func TestCRW1320_CapacityWaitAndPartialStartAreNotBlocked(t *testing.T) {
	t.Run("capacity", func(t *testing.T) {
		l := NewLab(t, apart()...)
		sender := l.wireUpward(t, true)
		l.Relay.HoldBack = "capacity_exhausted"
		got := mustRunIncomplete(t, l)
		for _, ln := range got.Lanes {
			if ln.State != "unreleased" || ln.Reason != "capacity_exhausted" {
				t.Fatalf("lanes %+v", got.Lanes)
			}
		}
		if d := flowcli.Detail(flowcli.RunRecord{RunID: l.RunID}, l.Events(), false, 0); d.State != flowcli.StateCompleted {
			t.Errorf("state %q", d.State)
		}
		if sent, rows := projectBlocked(l, sender); len(sent) != 0 || len(rows) != 0 {
			t.Errorf("project_blocked sent %+v rows %+v", sent, rows)
		}
	})
	t.Run("partial", func(t *testing.T) {
		l := NewLab(t, apart()...)
		sender := l.wireUpward(t, true)
		l.Relay.ReleaseErr = func(node string) error {
			if node == "B" {
				return errors.New("the relay refused the release of B: stale_coordinator_epoch")
			}
			return nil
		}
		got := mustRunIncomplete(t, l)
		if state, _ := laneOf(t, got, "A"); state != "landed" {
			t.Fatalf("lane A: %s", state)
		}
		d := flowcli.Detail(flowcli.RunRecord{RunID: l.RunID}, l.Events(), false, 0)
		if d.State != flowcli.StateCompleted || len(d.ItemErrors) != 1 || d.ItemErrors[0].Item != "B" {
			t.Errorf("state %q item errors %+v", d.State, d.ItemErrors)
		}
		if sent, rows := projectBlocked(l, sender); len(sent) != 0 || len(rows) != 0 {
			t.Errorf("project_blocked sent %+v rows %+v", sent, rows)
		}
	})
}
