package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// judgePanel is a generic script run: a judge panel that fans out with parallel, one judge throws, and the script never means to start
// a node of a plan (no crw.release, crw.releaseChild or crw.integrate).
const judgePanel = `export const meta = { name: 'judge-panel' }
const verdicts = await parallel([
  () => agent.call('judge one', { agentType: 'reviewer', label: 'judge-1' }),
  () => agent.call('judge two', { agentType: 'reviewer', label: 'judge-2' }),
  async () => { throw new Error('judge three gave no verdict') },
])
return verdicts.filter((v) => v !== null).length
`

// leadRecorder stands in for ` + "`crw manage send-parent`" + `: it records what the run tells the lead and its parent.
type leadRecorder struct {
	mu   sync.Mutex
	lead []report.SupervisorSend
}

func (p *leadRecorder) SendSupervisor(_ context.Context, s report.SupervisorSend) (report.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lead = append(p.lead, s)
	return report.SendResult{Class: "accepted", RequestID: "req-" + s.LogicalID}, nil
}

func (p *leadRecorder) SendParent(_ context.Context, m report.ParentSend) (report.SendResult, error) {
	return report.SendResult{Class: "queued", RequestID: "req-" + m.LogicalID}, nil
}

// genericRun runs the judge panel as a run process of the shipped binary does, with the hook the binary installs (UpwardFactory, its
// senders replaced by a recorder), under the arguments given; the record names the plan the arguments name, as RunIdentity does.
func genericRun(t *testing.T, id, args string) (*processWorld, *leadRecorder) {
	t.Helper()
	pw := newProcessWorld(t, judgePanel)
	lead := &leadRecorder{}
	prev := journal.UpwardFactory
	t.Cleanup(func() { journal.UpwardFactory = prev })
	journal.UpwardFactory = func(st *store.Store, j *journal.Journal, runID, state, socket string) engine.UpwardHook {
		h := UpwardFactory(st, j, runID, state, socket).(*report.RunHook)
		h.Sender = lead
		h.Reports.Sender = lead
		return h
	}
	pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		st, err := store.Open(ctx, filepath.Join(req.State, "relay.sqlite3"), "")
		if err != nil {
			return World{}, err
		}
		j, err := journal.Open(ctx, st, req.Spec.RunID, journal.Identity{ActorTaskID: req.Spec.Thread, PlanID: planOf(req.Spec.Args)}, journal.Options{})
		if err != nil {
			return World{}, err
		}
		fake := engine.NewFake()
		fake.Handler = func(engine.FakeRequest) engine.FakeReply {
			return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
		}
		return World{Journal: j, Backend: fake, Upward: journal.UpwardFactory(st, j, req.Spec.RunID, req.State, ""), Close: func() error { return errJoin(j.Close(), st.Close()) }}, nil
	}
	spec := pw.spec(id, func(s *Spec) {
		s.Workflow = "judge-panel"
		if args != "" {
			s.Args = json.RawMessage(args)
		}
	})
	if code, errOut := pw.execute(spec, false, nil); code != 0 {
		t.Fatalf("the run: exit %d: %s", code, errOut)
	}
	return pw, lead
}

// projectBlockedRows are the report rows of kind project_blocked of a run.
func projectBlockedRows(t *testing.T, w *world, id string) (rows []engine.Event) {
	t.Helper()
	events, _, err := loadRun(w, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == report.KindReport && str(e.Data, "report_kind") == report.KindProjectBlocked {
			rows = append(rows, e)
		}
	}
	return rows
}

// TestCRW1320_GenericScriptRunWithAFailedItemIsNotBlocked: a script run that is no lane workflow and names no plan (a judge panel whose
// one judge threw) started no node because it never meant to start one. It keeps today's state, completed, with its item error shown,
// show says nothing of a node, and the run sends no project_blocked report and writes no report row.
func TestCRW1320_GenericScriptRunWithAFailedItemIsNotBlocked(t *testing.T) {
	pw, lead := genericRun(t, "run-judges", "")
	info := pw.info("run-judges")
	if info.Status != engine.RunCompleted || info.State != StateCompleted || len(info.ItemErrors) != 1 || !strings.Contains(info.ItemErrors[0].Error, "judge three gave no verdict") {
		t.Errorf("state %q status %q item errors %+v", info.State, info.Status, info.ItemErrors)
	}
	_, out, _ := pw.td.do("show", "run-judges", "--state", pw.state)
	if !strings.Contains(out, "run run-judges  judge-panel  completed") || strings.Contains(out, "no node started") {
		t.Errorf("show:\n%s", out)
	}
	if rows := projectBlockedRows(t, pw.world, "run-judges"); len(rows) != 0 {
		t.Errorf("project_blocked rows %+v", rows)
	}
	if len(lead.lead) != 0 {
		t.Errorf("the lead was told %+v", lead.lead)
	}
}

// TestCRW1320_ScriptRunThatNamesAPlanAndStartedNoNodeIsBlocked: the same script run, when its arguments name a plan, is meant to start
// that plan's nodes: it is shown blocked and writes its one project_blocked report row (the plan is not in the store, so the row records
// the recipient it could not read).
func TestCRW1320_ScriptRunThatNamesAPlanAndStartedNoNodeIsBlocked(t *testing.T) {
	pw, _ := genericRun(t, "run-planned", `{"plan":"plan-9"}`)
	if info := pw.info("run-planned"); info.State != StateBlocked || info.PlanID != "plan-9" || len(info.ItemErrors) != 1 {
		t.Errorf("state %q plan %q item errors %+v", info.State, info.PlanID, info.ItemErrors)
	}
	if rows := projectBlockedRows(t, pw.world, "run-planned"); len(rows) != 1 {
		t.Errorf("project_blocked rows %+v", rows)
	}
}

// TestCRW1320_OnlyRunsMeantToStartNodesAreBlocked: the reading of Summarize over the same rows (no node started, an item failed): a run
// of a lane workflow (project-lane, issue-lane, night-batch) or a run that names a plan is blocked; another run that names no plan keeps
// the state completed.
func TestCRW1320_OnlyRunsMeantToStartNodesAreBlocked(t *testing.T) {
	w := newWorld(t)
	startedNone(t, w, "run-s", nil)
	events, _, err := loadRun(w, "run-s")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rec  RunRecord
		want string
	}{
		{RunRecord{RunID: "run-s", Workflow: "project-lane"}, StateBlocked},
		{RunRecord{RunID: "run-s", Workflow: "issue-lane"}, StateBlocked},
		{RunRecord{RunID: "run-s", Workflow: NightBatchWorkflow}, StateBlocked},
		{RunRecord{RunID: "run-s", Workflow: "judge-panel", PlanID: "plan-1"}, StateBlocked},
		{RunRecord{RunID: "run-s", Workflow: "judge-panel"}, StateCompleted},
		{RunRecord{RunID: "run-s", Workflow: "scout"}, StateCompleted},
	} {
		if info := Summarize(c.rec, events, false, 0); info.State != c.want || len(info.ItemErrors) != 2 {
			t.Errorf("%s plan %q: state %q item errors %d, want %s", c.rec.Workflow, c.rec.PlanID, info.State, len(info.ItemErrors), c.want)
		}
	}
}
