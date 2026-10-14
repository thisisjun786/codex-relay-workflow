package adapter

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1380 (CRW-1314 lane D2, design §3 and §7), end to end over the scripted App Server: a flow
// run's reports go up through the supervisor channel's direct staging (report.RelaySender), the
// daemon's own tick opens a turn in the idle management thread they are for, and the settle of the
// message writes the report_delivery row - once, in the transaction that settles it. Staging writes
// only queued. Nothing goes through `crw manage send-parent` or its parent-queue.

const flowRun, flowPlan = "run-d2", "plan-d2"

// flowWorld is a management world with a plan of project P-A, a flow run of it whose owner is
// ownerA, and the report writer of that run over the channel.
type flowWorld struct {
	*mgmtWorld
	fallback *refusingSender
}

// refusingSender is the fallback a report must never reach: it records what it was asked.
type refusingSender struct{ asked []string }

func (r *refusingSender) SendSupervisor(_ context.Context, s report.SupervisorSend) (report.SendResult, error) {
	r.asked = append(r.asked, "supervisor "+s.LogicalID)
	return report.SendResult{Class: "queued"}, nil
}

func (r *refusingSender) SendParent(_ context.Context, p report.ParentSend) (report.SendResult, error) {
	r.asked = append(r.asked, "parent "+p.LogicalID)
	return report.SendResult{Class: "queued"}, nil
}

// flowRun writes the plan of project and a flow run of it, owned by ownerA, into the opened store.
func (w *mgmtWorld) flowRun(project string) *flowWorld {
	w.t.Helper()
	raw, err := json.Marshal(map[string]any{"schema": dag.SchemaRevision, "plan_id": flowPlan, "project_key": project, "request_id": "r1",
		"expected_parent_revision": 0, "author_task_id": "task-test", "changes": []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc("A", dag.NodeNonPR)}}})
	if err != nil {
		w.t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := (&dag.Repo{Store: w.s}).Put(w.ctx, rev); err != nil {
		w.t.Fatal(err)
	}
	j, err := journal.Open(w.ctx, w.s, flowRun, journal.Identity{PlanID: flowPlan, PlanRevision: 1, ActorTaskID: ownerA, EpochNonce: "nonce"}, journal.Options{})
	if err != nil {
		w.t.Fatal(err)
	}
	j.Append(engine.Event{RunID: flowRun, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
	if err := j.Flush(); err != nil {
		w.t.Fatal(err)
	}
	return &flowWorld{mgmtWorld: w, fallback: &refusingSender{}}
}

func (f *flowWorld) writer() *report.Writer {
	return &report.Writer{Store: f.s, Rows: report.ForeignAppender{Store: f.s, Run: flowRun}, Resolver: report.StoreResolver{Store: f.s},
		Sender: report.RelaySender{Store: f.s, Fallback: f.fallback}, ArtifactDir: f.t.TempDir()}
}

// milestone closes milestone M1 of the run: the report the close sends up (report.Reporter, as the run's lane does).
func (f *flowWorld) milestone() string {
	f.t.Helper()
	id, err := report.Reporter{W: f.writer(), Run: flowRun}.ReportUp(f.ctx, ops.Report{Milestone: "M1", Plan: flowPlan, Range: ops.CloseRange{Base: "b0", Tip: "t1"}})
	if err != nil {
		f.t.Fatalf("the milestone report: %v", err)
	}
	return id
}

// deliveries are the report_delivery rows of a report, by result.
func (f *flowWorld) deliveries(id string) map[string]int {
	f.t.Helper()
	rows, err := report.ReadRows(f.ctx, f.s.DB, "", report.KindDelivery)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range rows {
		if r.Str("report_id") == id {
			out[r.Str("result")]++
		}
	}
	return out
}

// noParentQueue fails when anything was asked of crw manage send-parent or a parent-queue exists.
func (f *flowWorld) noParentQueue() {
	f.t.Helper()
	if len(f.fallback.asked) != 0 {
		f.t.Fatalf("the report went through send-parent: %v", f.fallback.asked)
	}
	_ = filepath.WalkDir(f.state, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "parent-queue" {
			f.t.Fatalf("a parent-queue was written: %s", path)
		}
		return nil
	})
}

// flowMessage is the one supervisor message of a kind: its id and state.
func (f *flowWorld) flowMessage(kind string) (string, string) {
	f.t.Helper()
	rows, err := f.s.All(f.ctx, "SELECT message_id, state, recipient_task_id FROM supervisor_messages WHERE obligation_kind = ?", kind)
	if err != nil || len(rows) != 1 {
		f.t.Fatalf("%d %s messages (%v)\n%s", len(rows), kind, err, f.dump())
	}
	return rows[0].Text("message_id"), rows[0].Text("state")
}

// TestFlowReportE2EMilestoneCompletionWakesTheIdleManagement: the project owner's milestone
// completion opens a new turn in the idle management thread above the project; the delivery
// record says so (one accepted report_delivery row after the queued one), and no parent-queue file
// is written. A replay of the report and more ticks change nothing.
func TestFlowReportE2EMilestoneCompletionWakesTheIdleManagement(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.open(mgmtA)
	f := w.flowRun("P-A")
	id := f.milestone()
	if got := f.deliveries(id); got["queued"] != 1 || len(got) != 1 {
		t.Fatalf("after staging the deliveries are %v, want one queued", got)
	}
	message, state := f.flowMessage("flow_milestone_report")
	if state != "queued" {
		t.Fatalf("the report's message is %s", state)
	}
	w.tick(3)
	turns := w.turnsOf(mgmtA)
	if len(turns) != 1 || !strings.Contains(turns[0], "Milestone M1 of plan "+flowPlan+" is closed") || !strings.Contains(turns[0], id) {
		t.Fatalf("management A's turns %q\n%s", turns, w.dump())
	}
	if got := f.deliveries(id); got["queued"] != 1 || got["accepted"] != 1 || len(got) != 2 {
		t.Fatalf("after the settle the deliveries are %v, want one queued and one accepted", got)
	}
	if state := w.messageState(message); state != "dispatched" {
		t.Fatalf("the message is %s", state)
	}
	views, err := report.ReportsMatching(w.ctx, w.s.DB, report.ReportFilter{Project: "P-A"})
	if err != nil || len(views) != 1 || views[0].Standing || views[0].LastDelivery != "accepted" {
		t.Fatalf("the report reads %+v (%v)", views, err)
	}
	// a resume of the run sends the same report again: it is a replay, nothing is staged, nothing written
	f.milestone()
	w.tick(2)
	if got := f.deliveries(id); got["queued"] != 1 || got["accepted"] != 1 || len(got) != 2 || len(w.turnsOf(mgmtA)) != 1 {
		t.Fatalf("after the replay the deliveries are %v and the turns %d", got, len(w.turnsOf(mgmtA)))
	}
	f.noParentQueue()
}

// TestFlowReportE2EStoreSeatReportIsDelivered: a project with no management edge falls to the
// store seat, and the report is delivered to it through the channel (D1c d2), its result on
// report_delivery once.
func TestFlowReportE2EStoreSeatReportIsDelivered(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "store", "--scope", "store", "--task", storeSeat, "--host", "host")
	w.bindOwner("P-C", ownerA, "")
	w.open(storeSeat)
	f := w.flowRun("P-C")
	id := f.milestone()
	w.tick(3)
	if got := w.turnsOf(storeSeat); len(got) != 1 || !strings.Contains(got[0], id) {
		t.Fatalf("the store seat's turns %q\n%s", got, w.dump())
	}
	w.tick(2)
	if got := f.deliveries(id); got["queued"] != 1 || got["accepted"] != 1 || len(got) != 2 {
		t.Fatalf("the deliveries are %v, want one queued and one accepted", got)
	}
	f.noParentQueue()
}

// TestFlowReportE2EDaemonDeathAndResumeNeverWriteTheDeliveryTwice: a daemon that dies after the
// transport and before its settle commits leaves no delivery row (the send is unknown and is not
// repeated); a daemon that dies after the settle leaves the one row, and the next daemon and a
// resume of the run write no second.
func TestFlowReportE2EDaemonDeathAndResumeNeverWriteTheDeliveryTwice(t *testing.T) {
	t.Run("before the settle", func(t *testing.T) {
		w := newMgmtWorld(t)
		w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
		w.bindOwner("P-A", ownerA, mgmtA)
		w.open(mgmtA)
		f := w.flowRun("P-A")
		id := f.milestone()
		dying, die := context.WithCancel(w.ctx)
		started := false
		w.host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
			started = true
			die()
			return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-dying"}}}
		})
		_, _ = w.d.Tick(dying)
		if !started {
			t.Fatalf("the transport never started\n%s", w.dump())
		}
		if got := f.deliveries(id); got["queued"] != 1 || len(got) != 1 {
			t.Fatalf("a daemon that died before its settle left the deliveries %v", got)
		}
		// the next daemon finds the stranded send once its lease is over: unknown, so not repeated
		w.clock.T += 400
		w.d = daemon.New(w.s, w.a, w.clock, w.c)
		w.tick(3)
		f.milestone()
		w.tick(2)
		if got := f.deliveries(id); got["queued"] != 1 || len(got) != 1 {
			t.Fatalf("after the restart and the resume the deliveries are %v, want the one queued", got)
		}
	})
	t.Run("after the settle", func(t *testing.T) {
		w := newMgmtWorld(t)
		w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
		w.bindOwner("P-A", ownerA, mgmtA)
		w.open(mgmtA)
		f := w.flowRun("P-A")
		id := f.milestone()
		w.tick(2)
		w.d = daemon.New(w.s, w.a, w.clock, w.c)
		w.tick(2)
		f.milestone()
		w.tick(2)
		if got := f.deliveries(id); got["queued"] != 1 || got["accepted"] != 1 || len(got) != 2 || len(w.turnsOf(mgmtA)) != 1 {
			t.Fatalf("the deliveries are %v and the turns %d, want one of each", got, len(w.turnsOf(mgmtA)))
		}
	})
}

// TestFlowRunWakeE2ESettlesUnderTheSameRule: the run: step wake (S3 StageWake) opens a turn in the
// run's owner, and its settle writes one accepted message_delivery row in the run's flow_events.
func TestFlowRunWakeE2ESettlesUnderTheSameRule(t *testing.T) {
	w := newMgmtWorld(t)
	w.open(ownerA)
	f := w.flowRun("P-A")
	wake := journal.StageWake(w.s)
	if err := wake(w.ctx, engine.SessionWake{Run: flowRun, Owner: ownerA, Batch: "b1", ID: "b1", Ops: []string{"call-1/t1-a1"}}); err != nil {
		t.Fatal(err)
	}
	w.tick(3)
	if got := w.turnsOf(ownerA); len(got) != 1 || !strings.Contains(got[0], "crw flow next "+flowRun) {
		t.Fatalf("the owner's turns %q\n%s", got, w.dump())
	}
	w.tick(2)
	rows, err := report.ReadRows(w.ctx, w.s.DB, flowRun, "message_delivery")
	if err != nil || len(rows) != 1 || rows[0].Str("result") != "accepted" || rows[0].Str("kind") != "session_step_waiting" || rows[0].Str("wake_id") != "b1" {
		t.Fatalf("the wake's delivery rows are %+v (%v)", rows, err)
	}
	f.noParentQueue()
}
