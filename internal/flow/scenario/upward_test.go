package scenario

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
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

// finishedOutcome is the outcome of the run's last finished row (CRW-1487), nil when it has none.
func finishedOutcome(t *testing.T, l *Lab) *engine.Outcome {
	t.Helper()
	var out *engine.Outcome
	found := false
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindRun && ev.Event == "finished" {
			out, found = engine.FinishedOutcome(ev.Data), true
		}
	}
	if !found {
		t.Fatal("the run has no finished row")
	}
	return out
}

// completionTexts are the texts of the completion notices the orchestrator thread was sent.
func completionTexts(s *scenarioSender) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.Parent {
		if p.Kind == supervisor.KindFlowCompletion {
			out = append(out, p.Text)
		}
	}
	return out
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
	// the finished row carries the milestone state the lane returned, and the completion notice tells it (CRW-1487)
	if o := finishedOutcome(t, l); o == nil || o.State != got.State || got.State != "incomplete" {
		t.Fatalf("the finished outcome %+v, the lane's state %q", o, got.State)
	}
	if done := completionTexts(sender); len(done) != 1 || !strings.Contains(done[0], "state=incomplete") || !strings.Contains(done[0], "lanes=A:escalated/blocked_scope") || strings.Contains(done[0], "status=completed") {
		t.Fatalf("the completion notices %q", done)
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

// TestDesignLoopCapsWarnAndTellTheOrchestrator (CRW-1327): the design loop of the project lane has two caps, AUDIT_ROUNDS rounds and a
// token cap: DESIGN_TOKEN_CAP tokens, or DESIGN_TOKEN_WARN of the run's token budget (budget.run, the budget its first process
// started with, which the run's journal fixes across resumes: CRW-1327) when that is lower, so a run without a budget has one too.
// The loop's tokens are those of its architect, audit and fix calls and of the explorers crw.explore spawns for its open questions.
// Reaching either cap writes a warning row (warn design_loop_cap) and tells the orchestrator thread: the round cap through the
// design_audit_rounds escalation CRW-1347 raises (the milestone stops there as it decided), the token cap through a
// design_loop_tokens escalation that asks nothing, and the loop goes on: the DESIGN is recorded and the lanes are released.
func TestDesignLoopCapsWarnAndTellTheOrchestrator(t *testing.T) {
	warned := func(l *Lab, cap string) bool {
		for _, ev := range l.FlowRows("log") {
			if m := str(ev.Data, "message"); strings.HasPrefix(m, "warn design_loop_cap") && strings.Contains(m, cap) {
				return true
			}
		}
		return false
	}
	told := func(s *scenarioSender, reason string) bool {
		for _, p := range s.Parent {
			if strings.HasPrefix(p.LogicalID, "esc-") && strings.Contains(p.Text, "reason="+reason) {
				return true
			}
		}
		return false
	}
	t.Run("rounds", func(t *testing.T) {
		l := NewLab(t, twoNodes()...)
		sender := l.wireUpward(t, false)
		l.Sequence("^design-audit:r\\d+$", func(*Call) fakehostPlan {
			return Say(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "no", "evidence": []string{}}}, "summary": "no"})
		})
		res := l.Run()
		if res.Status != engine.RunCompleted {
			t.Fatalf("run %s: %s", res.Status, res.Error)
		}
		// the finished row carries the milestone state the lane returned, and the completion notice tells it (CRW-1487)
		if o, got := finishedOutcome(t, l), Value[laneResult](t, res); o == nil || o.State != got.State || got.State != "escalated" || o.Reason != "design_audit_rounds" {
			t.Errorf("the finished outcome %+v, the lane's state %q", o, got.State)
		}
		if done := completionTexts(sender); len(done) != 1 || !strings.Contains(done[0], "state=escalated reason=design_audit_rounds") || strings.Contains(done[0], "status=completed") {
			t.Errorf("the completion notices %q", done)
		}
		if !warned(l, "rounds") || !slices.Contains(escalationReasons(l), "design_audit_rounds") || !told(sender, "design_audit_rounds") {
			t.Errorf("the round cap: warned %v, escalations %v, told %+v", warned(l, "rounds"), escalationReasons(l), sender.Parent)
		}
	})
	t.Run("tokens", func(t *testing.T) {
		l := NewLab(t, twoNodes()...)
		sender := l.wireUpward(t, false)
		// the two background explorers are 2 (outside the design loop, CRW-1466) and the architect and the audit 20: the loop's 20 is 80%
		// of the budget, spent before any executor
		l.Budget = 25
		if res := l.Run(); res.Status != engine.RunCompleted {
			t.Fatalf("run %s: %s", res.Status, res.Error)
		}
		if !warned(l, "tokens") || !slices.Contains(escalationReasons(l), "design_loop_tokens") || !told(sender, "design_loop_tokens") {
			t.Errorf("the token cap: warned %v, escalations %v, told %+v", warned(l, "tokens"), escalationReasons(l), sender.Parent)
		}
		for _, q := range flowcli.Questions(l.Events()) {
			if q.Reason == "design_loop_tokens" {
				t.Errorf("the token cap asks a question: %+v", q)
			}
		}
		if len(l.Rows("crw.designLedger")) == 0 || len(l.Rows("crw.release")) == 0 {
			t.Errorf("the token cap blocked the loop: %d ledger rows, %d releases", len(l.Rows("crw.designLedger")), len(l.Rows("crw.release")))
		}
	})
	// the 10-13 field runs had no budget: an architect call of 6M tokens is past DESIGN_TOKEN_CAP (5M) on its own
	t.Run("tokens without a budget", func(t *testing.T) {
		l := NewLab(t, twoNodes()...)
		sender := l.wireUpward(t, false)
		l.On("^architect$", func(c *Call) *fakehostPlan {
			p := Say(l.designFor())
			p.Tokens = 6_000_000
			return &p
		})
		if res := l.Run(); res.Status != engine.RunCompleted {
			t.Fatalf("run %s: %s", res.Status, res.Error)
		}
		if !warned(l, "tokens") || !slices.Contains(escalationReasons(l), "design_loop_tokens") || !told(sender, "design_loop_tokens") {
			t.Errorf("the token cap of a run without a budget: warned %v, escalations %v, told %+v", warned(l, "tokens"), escalationReasons(l), sender.Parent)
		}
		if len(l.Rows("crw.designLedger")) == 0 || len(l.Rows("crw.release")) == 0 {
			t.Errorf("the token cap blocked the loop: %d ledger rows, %d releases", len(l.Rows("crw.designLedger")), len(l.Rows("crw.release")))
		}
	})
	// the explorers of the loop's open questions are the loop's tokens: the architect and the audit are 20 of 0.8 * 1500 = 1200, the
	// explorer's 1200 reach it
	t.Run("explorer tokens", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		sender := l.wireUpward(t, false)
		d := l.designFor()
		d.OpenQuestions = openQuestions(1)
		l.Design = &d
		l.Budget = 1500
		l.On("^explore:", func(c *Call) *fakehostPlan {
			p := Say(map[string]any{"answer": "answer of the explorer", "evidence": []string{"README.md"}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}})
			p.Tokens = 1200
			return &p
		})
		if res := l.Run(); res.Status != engine.RunCompleted {
			t.Fatalf("run %s: %s", res.Status, res.Error)
		}
		if len(l.CallsOf("explorer")) != 1 {
			t.Fatalf("%d explorer calls, want 1", len(l.CallsOf("explorer")))
		}
		if !warned(l, "tokens") || !slices.Contains(escalationReasons(l), "design_loop_tokens") || !told(sender, "design_loop_tokens") {
			t.Errorf("the explorer tokens of the loop: warned %v, escalations %v, told %+v", warned(l, "tokens"), escalationReasons(l), sender.Parent)
		}
	})
	// the token cap is the one the run's journal fixes (budget.run, the budget its first process started with), so a resume under
	// another ceiling replays the loop's design_loop_tokens escalation at the same position with the same input and goes on replaying:
	// it sends no turn and runs no op live (Argon F2 on 2a082733). The first process's ceiling is 10000 (cap 8000): the architect's 7995
	// and the first audit's 10 reach it at the audit, and the run completes within its budget. The resumes: the same ceiling (the
	// control), 10005 (cap 8004: the same call reaches it, under another text), 9000 (cap 7200: the architect alone would reach it),
	// 1000000 (the cap of 5000000 is never reached) and none.
	t.Run("tokens replayed under another budget", func(t *testing.T) {
		for _, budget := range []int64{10000, 10005, 9000, 1000000, 0} {
			t.Run(fmt.Sprint(budget), func(t *testing.T) {
				l := NewLab(t, twoNodes()...)
				l.On("^architect$", func(c *Call) *fakehostPlan {
					p := Say(l.designFor())
					p.Tokens = 7995
					return &p
				})
				l.Budget = 10000
				if res := l.Run(); res.Status != engine.RunCompleted {
					t.Fatalf("run %s: %s", res.Status, res.Error)
				}
				if !slices.Contains(escalationReasons(l), "design_loop_tokens") {
					t.Fatalf("the first process raised no design_loop_tokens: %v", escalationReasons(l))
				}
				if !warned(l, "used 8005 tokens by audit round 1") {
					t.Fatalf("the first process did not reach the cap at the first audit: %v", l.FlowRows("log"))
				}
				escalations := len(l.FlowRows(engine.EvEscalation))
				before := l.LastSeq()
				l.Budget = budget
				l.NewProcess("proc-budget")
				res := l.Resume()
				if res.Status != engine.RunCompleted {
					t.Fatalf("the resume under the ceiling %d: run %s: %s", budget, res.Status, res.Error)
				}
				if live := turnsSince(l, before); len(live) != 0 {
					var labels []string
					for _, ts := range live {
						labels = append(labels, labelOf(l, ts.call))
					}
					t.Errorf("the resume under the ceiling %d sent turns %v: the design loop did not replay", budget, labels)
				}
				var ran []string
				for _, ev := range rowsAfter(l, before) {
					if ev.Kind == engine.KindCall && (ev.Event == engine.EvCompleted || ev.Event == engine.EvSuperseded) {
						ran = append(ran, ev.Op+" "+ev.Event)
					}
				}
				if len(ran) != 0 {
					t.Errorf("the resume under the ceiling %d wrote call rows %v: a position of the run was not replayed", budget, ran)
				}
				if n := len(l.FlowRows(engine.EvEscalation)); n != escalations {
					t.Errorf("the resume under the ceiling %d: %d escalation rows, the first process wrote %d", budget, n, escalations)
				}
			})
		}
	})
}

// Cached input remains in the usage journal, but must not spend the design loop's token allowance.
// Exercise every source of design usage, including a null-model explorer's repair and fresh retry.
func TestDesignLoopCacheDoesNotEscalate(t *testing.T) {
	for _, stage := range []string{"architect", "audit", "explorer", "answers", "fix", "explorer retry"} {
		t.Run(stage, func(t *testing.T) {
			l := NewLab(t, oneNode()...)
			sender := l.wireUpward(t, false)
			approved := map[string]any{"verdict": "approved", "findings": []any{}, "summary": "approved"}
			answer := map[string]any{"answer": "answer", "evidence": []string{"README.md"}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}}
			cachedPlan := func(v any) *fakehostPlan {
				p := Say(v)
				// 4,000,000 input - 3,999,980 cached + 4,000,000 output = 4,000,020 counted.
				p.Tokens, p.CachedInputTokens = 8_000_000, 3_999_980
				return &p
			}
			if stage == "explorer" || stage == "answers" || stage == "explorer retry" {
				d := l.designFor()
				d.OpenQuestions = openQuestions(1)
				l.Design = &d
			}
			wantCached := int64(3_999_980)
			switch stage {
			case "architect":
				l.On("^architect$", func(*Call) *fakehostPlan { return cachedPlan(l.designFor()) })
			case "audit":
				l.On("^design-audit:r1$", func(*Call) *fakehostPlan { return cachedPlan(approved) })
			case "explorer":
				l.On("^explore:", func(*Call) *fakehostPlan { return cachedPlan(answer) })
			case "answers":
				l.On("^design-answers:", func(*Call) *fakehostPlan { return cachedPlan(l.designFor()) })
			case "fix":
				l.Sequence("^design-audit:r\\d+$", designAuditNeedsChanges, func(*Call) fakehostPlan { return Say(approved) })
				l.On("^design-fix:r1$", func(*Call) *fakehostPlan { return cachedPlan(l.designFor()) })
			case "explorer retry":
				// Two turns in the null-model call, one in its successful retry:
				// 9,000,000 total - 4,499,970 cached = 4,500,030 counted.
				wantCached = 4_499_970
				l.On("^explore:", func(c *Call) *fakehostPlan {
					p := nullModelTurns(c)
					if strings.HasSuffix(c.Label, ":retry") {
						p = cachedPlan(answer)
					}
					p.Tokens, p.CachedInputTokens = 3_000_000, 1_499_990
					return p
				})
			}
			res := l.Run()
			if got := Value[laneResult](t, res); got.State != "closed" {
				t.Fatalf("the design did not finish: %+v", got)
			}
			if slices.Contains(escalationReasons(l), "design_loop_tokens") {
				t.Errorf("cached input triggered design_loop_tokens: %v", l.FlowRows(engine.EvEscalation))
			}
			for _, ev := range l.FlowRows("log") {
				if text := str(ev.Data, "message"); strings.HasPrefix(text, "warn design_loop_cap tokens:") {
					t.Errorf("cached input caused a design warning: %s", text)
				}
			}
			for _, notice := range sender.Parent {
				if strings.Contains(notice.Text, "reason=design_loop_tokens") {
					t.Errorf("the orchestrator was escalated for cached input: %s", notice.Text)
				}
			}
			var cached int64
			for _, ev := range l.Events() {
				cached += engine.TerminalTokens(ev).CachedInput
			}
			if cached != wantCached {
				t.Errorf("journal cached input = %d, want %d recorded separately", cached, wantCached)
			}
			if len(l.Rows("crw.designLedger")) == 0 || len(l.Rows("crw.release")) == 0 {
				t.Error("the design did not reach its ledger and release")
			}
		})
	}
}

func TestDesignLoopNonCachedCapExplainsAccountingAndReplays(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.wireUpward(t, false)
	l.On("^architect$", func(*Call) *fakehostPlan {
		p := Say(l.designFor())
		// Input 4,999,970 - cache 4,999,950 + output 4,999,970 = 4,999,990.
		// The first audit's 10 tokens reach exactly the unchanged 5,000,000 cap.
		p.Tokens, p.CachedInputTokens = 9_999_940, 4_999_950
		return &p
	})
	if got := Value[laneResult](t, l.Run()); got.State != "closed" {
		t.Fatalf("the token warning blocked the design: %+v", got)
	}
	var details []string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "reason") == "design_loop_tokens" {
			details = append(details, str(ev.Data, "detail"))
		}
	}
	if len(details) != 1 {
		t.Fatalf("design token escalations %q, want exactly one", details)
	}
	for _, want := range []string{"used 5000000 tokens by audit round 1", "uncached input + output", "cached input 4999950 excluded"} {
		if !strings.Contains(details[0], want) {
			t.Errorf("detail %q does not explain %q", details[0], want)
		}
	}
	before := l.LastSeq()
	escalations := len(l.FlowRows(engine.EvEscalation))
	l.Budget = 12_000_000
	l.NewProcess("proc-cached-budget")
	if got := Value[laneResult](t, l.Resume()); got.State != "closed" {
		t.Fatalf("resume: %+v", got)
	}
	if live := turnsSince(l, before); len(live) != 0 {
		t.Errorf("cached usage replay sent %d live turns", len(live))
	}
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && (ev.Event == engine.EvCompleted || ev.Event == engine.EvSuperseded) {
			t.Errorf("cached usage replay ran %s (%s)", ev.Op, ev.Event)
		}
	}
	if got := len(l.FlowRows(engine.EvEscalation)); got != escalations {
		t.Errorf("replay changed escalation count from %d to %d", escalations, got)
	}
}
