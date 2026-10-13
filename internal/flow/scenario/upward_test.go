package scenario

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// scenarioSender stands in for crw manage send-parent in the scenarios: it records the notices of the upward half of the run in the order
// they were sent.
type scenarioSender struct {
	mu     sync.Mutex
	Events []string
	Parent []report.ParentSend
	Lead   []report.SupervisorSend
}

func (s *scenarioSender) SendSupervisor(_ context.Context, m report.SupervisorSend) (report.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Lead = append(s.Lead, m)
	s.Events = append(s.Events, "lead:"+m.LogicalID)
	return report.SendResult{Class: "accepted", RequestID: "req-" + m.LogicalID}, nil
}

func (s *scenarioSender) SendParent(_ context.Context, m report.ParentSend) (report.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Parent = append(s.Parent, m)
	s.Events = append(s.Events, "orchestrator:"+m.LogicalID)
	return report.SendResult{Class: "queued", RequestID: "req-" + m.LogicalID}, nil
}

const (
	upProject    = "PRJ-1"
	upInitiative = "INI-1"
	upLead       = "lead-1"
	upDocument   = "doc-1"
)

// wireUpward seeds what the upward half needs in the lab's store - the plan of the project, its registered parent (the lab's orchestrator
// task) and the initiative lead above it - and installs the hook and the reporter on the lab's run.
func (l *Lab) wireUpward(t *testing.T, lead bool) *scenarioSender {
	t.Helper()
	ctx := context.Background()
	nodes := []any{}
	for _, n := range l.Nodes {
		nodes = append(nodes, map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc(n.ID, dag.NodeNonPR)})
	}
	raw, err := json.Marshal(map[string]any{"schema": dag.SchemaRevision, "plan_id": "plan-1", "project_key": upProject, "request_id": "r1", "expected_parent_revision": 0, "author_task_id": "task-test", "changes": nodes})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&dag.Repo{Store: l.Store}).Put(ctx, rev); err != nil {
		t.Fatal(err)
	}
	bind := func(id, role, kind, key, task string) {
		if err := storeseed.InsertScopeBinding(ctx, l.Store, store.ScopeBindingsRow{BindingID: id, Role: role, ScopeKind: kind, ScopeKey: key, TaskID: task, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	bind("bnd-parent", "parent", "project", upProject, "parent")
	if lead {
		bind("bnd-lead", "supervisor", "initiative", upInitiative, upLead)
		if err := storeseed.InsertScopeLink(ctx, l.Store, store.ScopeLinksRow{LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: upInitiative, UpperTaskID: upLead,
			LowerKind: "project", LowerKey: upProject, LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	sender := &scenarioSender{}
	l.hookUpward(t, sender)
	return sender
}

// hookUpward installs the hook and the reporter on the lab's run over its current journal, as the binary does (flowcli.UpwardFactory): a
// resumed process (NewProcess) is hooked again with the same sender. The hook reports a run that started no node with the reading of
// `crw flow show` over the run's record (flowcli.RunBlocked).
func (l *Lab) hookUpward(t *testing.T, sender *scenarioSender) {
	t.Helper()
	rows := report.JournalAppender{Journal: l.journal, RunID: l.RunID, Store: l.Store}
	w := &report.Writer{Store: l.Store, Rows: rows, Resolver: report.StoreResolver{Store: l.Store}, Sender: sender, ArtifactDir: t.TempDir()}
	l.Reporter = report.Reporter{W: w, Run: l.RunID}
	l.Upward = &report.RunHook{Store: l.Store, Sched: &dagsched.Scheduler{Store: l.Store}, Journal: l.journal, Rows: rows, Sender: sender, Run: l.RunID,
		Reports: w, Blocked: flowcli.RunBlocked}
	l.newLane()
}

// An agent node's blocked signal climbs: the executor's RECEIPT says blocked_scope, the run escalates (its row), the escalation is told to
// the orchestrator thread that started the run - queued, with the run's own id and the reason - and the orchestrator, who owns the project,
// lifts it to the initiative lead with `crw flow report-up --kind project_blocked`: a report row and a notice to the lead's thread, after
// the orchestrator was told. The run itself wrote no report to the lead.
func TestAgentBlockedRoutesToOrchestratorThenUp(t *testing.T) {
	l := NewLab(t, oneNode()...)
	sender := l.wireUpward(t, true)
	l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
		return Say(ReceiptWith("blocked_scope", map[string]any{"gap": "the change needs the other package"}))
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "blocked_scope" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	esc := l.FlowRows(engine.EvEscalation)
	if len(esc) != 1 || str(esc[0].Data, "reason") != "blocked_scope" {
		t.Fatalf("escalations %+v", esc)
	}
	// the escalation went to the orchestrator thread, queued
	var told *report.ParentSend
	for i := range sender.Parent {
		if strings.HasPrefix(sender.Parent[i].LogicalID, "esc-") {
			told = &sender.Parent[i]
		}
	}
	if told == nil || told.Thread != "parent" || !told.Queue || !strings.Contains(told.Text, "reason=blocked_scope") || !strings.Contains(told.Text, "node=A") || !strings.Contains(told.Text, "the change needs the other package") {
		t.Fatalf("the orchestrator was sent %+v", sender.Parent)
	}
	if len(sender.Lead) != 0 {
		t.Fatalf("the run reported to the lead on its own: %+v", sender.Lead)
	}
	// the orchestrator lifts the project-level blocker
	w := &report.Writer{Store: l.Store, Rows: report.ForeignAppender{Store: l.Store, Run: l.RunID}, Resolver: report.StoreResolver{Store: l.Store}, Sender: sender, ArtifactDir: t.TempDir()}
	res, err := w.Up(context.Background(), report.UpRequest{Kind: report.KindProjectBlocked, Run: l.RunID, Subject: "node A needs the other package", Body: []byte("blocked_scope on A: " + str(esc[0].Data, "detail"))})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivery != report.ResultAccepted || res.Recipient.Thread != upLead || res.Recipient.InitiativeKey != upInitiative {
		t.Fatalf("report-up %+v", res)
	}
	if len(sender.Lead) != 1 || sender.Lead[0].Thread != upLead || !strings.Contains(sender.Lead[0].Text, "project_blocked") {
		t.Fatalf("the lead was sent %+v", sender.Lead)
	}
	order := slices.Clone(sender.Events)
	escAt, leadAt := -1, -1
	for i, e := range order {
		if strings.HasPrefix(e, "orchestrator:esc-") && escAt < 0 {
			escAt = i
		}
		if strings.HasPrefix(e, "lead:") {
			leadAt = i
		}
	}
	if escAt < 0 || leadAt < 0 || escAt > leadAt {
		t.Errorf("the order of the notices: %v", order)
	}
}

// The milestone close of the project lane reports up through the reporter: the report row is in the run's journal, written before the
// notice to the lead, and the operation's row names it.
func TestMilestoneCloseReportsUpThroughTheReporter(t *testing.T) {
	l := NewLab(t, oneNode()...)
	sender := l.wireUpward(t, true)
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	rows := l.Rows("crw.reportUp")
	if len(rows) != 1 || str(rows[0].Data, "report_id") == "" || rows[0].Data["delivered"] != true {
		t.Fatalf("reportUp rows %+v", rows)
	}
	reports := l.FlowRows(report.KindReport)
	if len(reports) != 1 || str(reports[0].Data, "report_id") != str(rows[0].Data, "report_id") || str(reports[0].Data, "recipient_thread") != upLead || str(reports[0].Data, "report_kind") != report.KindMilestoneReport {
		t.Fatalf("report rows %+v", reports)
	}
	if len(sender.Lead) != 1 || sender.Lead[0].LogicalID != l.RunID+":M1:report" {
		t.Fatalf("the lead was sent %+v", sender.Lead)
	}
	if d := l.FlowRows(report.KindDelivery); len(d) != 1 || str(d[0].Data, "result") != "accepted" {
		t.Fatalf("delivery rows %+v", d)
	}
}
