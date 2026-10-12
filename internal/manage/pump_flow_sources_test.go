package manage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// pumpFlowTestReal puts the real read of the relay store back (the quiet default of the pump tests finds nothing) and points it at the
// store of the test.
func pumpFlowTestReal(t *testing.T) {
	t.Helper()
	saved := pumpFlowRead
	pumpFlowRead = func(ctx context.Context, _ *Env, cfg *Config) (pumpFlowData, error) {
		return pumpFlowReadAt(ctx, cfg.Relay.State)
	}
	t.Cleanup(func() { pumpFlowRead = saved })
}

// pumpFlowTestMessages are the texts of the messages the management thread was sent.
func pumpFlowTestMessages(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, call := range supervisorTestCallsTo(t, log, deliverToolSend, "mgmt-thread") {
		out = append(out, call["args"].(map[string]any)["message"].(string))
	}
	return out
}

func pumpFlowTestRound(t *testing.T, e *Env, cfg *Config) {
	t.Helper()
	if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil || code != 0 {
		t.Fatalf("round: %d %v", code, err)
	}
}

// pumpFlowTestSetup is a pump over a relay store with flow rows, a fake bridge that takes every message to the management thread, and the
// flow sources only.
func pumpFlowTestSetup(t *testing.T, now *time.Time, sources ...pumpSource) (*Env, *Config, *store.Store, string) {
	t.Helper()
	e := pumpTestEnv(t, now)
	pumpFlowTestReal(t)
	pumpTestSwapSources(t, sources...)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	state, st := flowTestStore(t)
	cfg.Relay.State = state
	return e, cfg, st, log
}

// A run that started, did not finish and has written no row of its own for run_dead_seconds is dead: the management thread is told once,
// at once (urgent), and told again only when the run is resumed and dies again. A row a command wrote into the run is no sign of life.
func TestPumpRunDeadNotice(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowRunsSource{})
	flowTestRun(t, st, "run-dead", "orch-task", now.Add(-2*time.Hour), false)
	flowTestRun(t, st, "run-live", "orch-task", now.Add(-time.Minute), false)
	flowTestRun(t, st, "run-done", "orch-task", now.Add(-3*time.Hour), true)
	// a report a command wrote just now says nothing about the process of the dead run
	flowTestRow(t, st, "run-dead", "report", map[string]any{"report_id": "rep_1", "report_kind": "milestone_report"}, now.Add(-time.Second))

	pumpFlowTestRound(t, e, cfg)
	msgs := pumpFlowTestMessages(t, log)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "run dead run=run-dead") || !strings.Contains(msgs[0], "actor=orch-task") || !strings.Contains(msgs[0], "(urgent)") || strings.Contains(msgs[0], "run-live") || strings.Contains(msgs[0], "run-done") {
		t.Fatalf("messages %q", msgs)
	}
	pumpFlowTestRound(t, e, cfg)
	if got := pumpFlowTestMessages(t, log); len(got) != 1 {
		t.Fatalf("the same death was told again: %q", got)
	}
	// resumed, and dead again: another event
	now = now.Add(time.Hour)
	flowTestRun(t, st, "run-dead", "orch-task", now.Add(-time.Minute), false)
	now = now.Add(2 * time.Hour)
	pumpFlowTestRound(t, e, cfg)
	got := pumpFlowTestMessages(t, log)
	var dead int
	for _, m := range got {
		if strings.Contains(m, "run dead run=run-dead") {
			dead++
		}
	}
	if dead != 2 {
		t.Fatalf("the second death was not told: %q", got)
	}
}

// A turn of an agent writes no row of the run while it is in flight, so a run with an open agent call (prepared or thread_started, no
// terminal row) is not dead after run_dead_seconds: it is dead only after the longer grace of an open call. A run whose call ended is
// judged by run_dead_seconds again.
func TestPumpRunDeadWaitsForAnOpenAgentCall(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowRunsSource{})
	callRow := func(run, call, event string, at time.Time) {
		t.Helper()
		j, err := journal.Open(context.Background(), st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "orch-task", EpochNonce: "n"}, journal.Options{Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		j.Append(engine.Event{RunID: run, Kind: engine.KindCall, CallID: call, Path: "/", Position: 1, Op: "agent", Event: event, Data: map[string]any{"role": "executor", "thread_id": "t-" + call}})
		if err := j.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	flowTestRun(t, st, "run-turn", "orch-task", now.Add(-3*time.Hour), false)
	callRow("run-turn", "call-1", engine.EvPrepared, now.Add(-3*time.Hour))
	callRow("run-turn", "call-1", engine.EvThreadStarted, now.Add(-3*time.Hour))
	flowTestRun(t, st, "run-over", "orch-task", now.Add(-3*time.Hour), false)
	callRow("run-over", "call-2", engine.EvPrepared, now.Add(-3*time.Hour))
	callRow("run-over", "call-2", engine.EvCompleted, now.Add(-3*time.Hour))
	pumpFlowTestRound(t, e, cfg)
	msgs := pumpFlowTestMessages(t, log)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "run dead run=run-over") || strings.Contains(msgs[0], "run-turn") {
		t.Fatalf("messages %q", msgs)
	}
	// the open call is quiet for longer than any turn: dead
	now = now.Add(pumpFlowOpenCallGrace)
	pumpFlowTestRound(t, e, cfg)
	var turn int
	for _, m := range pumpFlowTestMessages(t, log) {
		if strings.Contains(m, "run dead run=run-turn") {
			turn++
		}
	}
	if turn != 1 {
		t.Fatalf("the run with an open call was not told once: %q", pumpFlowTestMessages(t, log))
	}
}

// An escalation row a run writes is told to the management thread as run_escalation, urgent. The first read only notes where the rows end,
// so the history of the store is not sent.
func TestPumpRunEscalationNotice(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowRunsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now, false)
	flowTestRow(t, st, "run-1", "escalation", map[string]any{"escalation_id": "esc_old", "reason": "tiers_exhausted", "class": "tiers", "node": "A"}, now)
	pumpFlowTestRound(t, e, cfg)
	if got := pumpFlowTestMessages(t, log); len(got) != 0 {
		t.Fatalf("history was sent: %q", got)
	}
	flowTestRow(t, st, "run-1", "escalation", map[string]any{"escalation_id": "esc_new", "reason": "blocked_scope", "class": "scope", "node": "B", "detail": "the gap\nis in b/"}, now)
	pumpFlowTestRound(t, e, cfg)
	got := pumpFlowTestMessages(t, log)
	if len(got) != 1 || !strings.Contains(got[0], "run escalation run=run-1 node=B reason=blocked_scope class=scope") || !strings.Contains(got[0], "the gap is in b/") || strings.Contains(got[0], "esc_old") {
		t.Fatalf("messages %q", got)
	}
	pumpFlowTestRound(t, e, cfg)
	if got := pumpFlowTestMessages(t, log); len(got) != 1 {
		t.Fatalf("told again: %q", got)
	}
}

// A report nobody read is told to the management thread as the low kind report_unread - the report, its age and the last delivery result
// - once it is low_priority_seconds old, and the low kind waits low_priority_seconds more as every low event does. A read report, and one
// that is young, are not told.
func TestUnreadReportVisibleToManager(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowReportsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now.Add(-3*time.Hour), false)
	row := func(kind string, payload map[string]any, at time.Time) {
		flowTestRow(t, st, "run-1", kind, payload, at)
	}
	row("report", map[string]any{"report_id": "rep_a", "report_kind": "milestone_report", "milestone": "M1"}, now.Add(-30*time.Minute))
	row("report_delivery", map[string]any{"report_id": "rep_a", "result": "queued"}, now.Add(-29*time.Minute))
	row("report", map[string]any{"report_id": "rep_b", "report_kind": "milestone_report", "milestone": "M0"}, now.Add(-2*time.Hour))
	row("report_read", map[string]any{"report_id": "rep_b", "turn": "t1"}, now.Add(-time.Hour))
	row("report", map[string]any{"report_id": "rep_c", "report_kind": "project_blocked"}, now.Add(-time.Minute))

	pumpFlowTestRound(t, e, cfg)
	if got := pumpFlowTestMessages(t, log); len(got) != 0 {
		t.Fatalf("a low event was sent at once: %q", got)
	}
	st1 := pumpTestReadState(t, cfg)
	if len(st1.Pending) != 1 || st1.Pending[0].Kind != pumpKindReportUnread || st1.Pending[0].ID != "report_unread:rep_a" {
		t.Fatalf("pending %+v", st1.Pending)
	}
	now = now.Add(16 * time.Minute)
	pumpFlowTestRound(t, e, cfg)
	got := pumpFlowTestMessages(t, log)
	if len(got) != 1 || !strings.Contains(got[0], "report unread report=rep_a kind=milestone_report run=run-1 age=") || !strings.Contains(got[0], "last delivery=queued") || strings.Contains(got[0], "rep_b") || strings.Contains(got[0], "(urgent)") {
		t.Fatalf("messages %q", got)
	}
}

// A decision only Jun can make is a report row whose recipient is the store seat; the pump tells the management thread at once, as the
// urgent kind jun_decision, and tells it once.
func TestJunDecisionRoutesToManagerPump(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowReportsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now, false)
	flowTestRow(t, st, "run-1", "report", map[string]any{"report_id": "rep_j", "report_kind": "jun_decision_request", "subject": "Ship the cut before the review?"}, now)
	pumpFlowTestRound(t, e, cfg)
	got := pumpFlowTestMessages(t, log)
	if len(got) != 1 || !strings.Contains(got[0], "decision for Jun report=rep_j run=run-1") || !strings.Contains(got[0], "Ship the cut before the review?") || !strings.Contains(got[0], "(urgent)") {
		t.Fatalf("messages %q", got)
	}
	pumpFlowTestRound(t, e, cfg)
	if got := pumpFlowTestMessages(t, log); len(got) != 1 {
		t.Fatalf("told again: %q", got)
	}
}

// A store the pump cannot read leaves the sources unmeasured and sends nothing.
func TestPumpFlowSourcesAreUnmeasuredWithoutAStore(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, _, log := pumpFlowTestSetup(t, &now, pumpFlowRunsSource{}, pumpFlowReportsSource{})
	cfg.Relay.State = t.TempDir()
	pumpFlowTestRound(t, e, cfg)
	if got := pumpFlowTestMessages(t, log); len(got) != 0 {
		t.Fatalf("messages %q", got)
	}
	st := pumpTestReadState(t, cfg)
	if st.Sources[pumpFlowRunsName] != pumpSourceUnmeasured || st.Sources[pumpFlowReportsName] != pumpSourceUnmeasured {
		t.Errorf("sources %v", st.Sources)
	}
}

// A run that returned from its first process (a held or incomplete workflow leaves a finished row) and was resumed is alive again until
// its new process finishes: when that process dies, the run is dead, whatever finished row its history holds. A run whose newest process
// finished is not. (post-evaluation d5)
func TestPumpRunDeadSeesAResumedRunThatDiedAfterAnEarlierFinish(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowRunsSource{})
	// first process finished at -5h; resumed at -2h; the second process died
	flowTestRun(t, st, "run-resumed-dead", "orch-task", now.Add(-5*time.Hour), true)
	flowTestRun(t, st, "run-resumed-dead", "orch-task", now.Add(-2*time.Hour), false)
	// first process finished, resumed, and the second finished too
	flowTestRun(t, st, "run-resumed-done", "orch-task", now.Add(-5*time.Hour), true)
	flowTestRun(t, st, "run-resumed-done", "orch-task", now.Add(-4*time.Hour), true)
	pumpFlowTestRound(t, e, cfg)
	msgs := pumpFlowTestMessages(t, log)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "run dead run=run-resumed-dead") {
		t.Fatalf("messages %q", msgs)
	}
}
