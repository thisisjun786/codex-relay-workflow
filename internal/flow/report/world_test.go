package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

const (
	testProject    = "PRJ-1"
	testInitiative = "INI-1"
	testPlan       = "plan-1"
	testRun        = "run-1"
	testOrch       = "orch-task"
	testLead       = "lead-1"
	testDocument   = "doc-1"
)

// world is a relay store with a plan of a project, the project's registered parent (the orchestrator) and a run of that plan: the
// hierarchy a milestone report climbs.
type world struct {
	t       *testing.T
	st      *store.Store
	sched   *dagsched.Scheduler
	journal *journal.Journal
	sender  *fakeSender
	repo    *dag.Repo
	nodes   int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	st := newStore(t)
	w := &world{t: t, st: st, sender: &fakeSender{}, repo: &dag.Repo{Store: st}}
	w.sched = &dagsched.Scheduler{Store: st}
	w.putPlan(0, "r1", testsupport.NodeDoc("A", dag.NodeNonPR), testsupport.NodeDoc("B", dag.NodeNonPR))
	w.bind("bnd-parent", "parent", "project", testProject, testOrch)
	w.journal = seedRun(t, st, testRun, testPlan, testOrch)
	return w
}

// putPlan writes a revision of the plan that adds the nodes.
func (w *world) putPlan(parent int, request string, nodes ...map[string]any) {
	w.t.Helper()
	changes := make([]any, len(nodes))
	for i, n := range nodes {
		changes[i] = map[string]any{"op": dag.OpAddNode, "node": n}
	}
	raw, err := json.Marshal(map[string]any{"schema": dag.SchemaRevision, "plan_id": testPlan, "project_key": testProject, "request_id": request,
		"expected_parent_revision": parent, "author_task_id": "task-test", "changes": changes})
	if err != nil {
		w.t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.repo.Put(ctx, rev); err != nil {
		w.t.Fatal(err)
	}
}

// bump moves the plan on by a node, so the summary of the plan differs from every earlier state.
func (w *world) bump() {
	w.t.Helper()
	snap, _, err := w.repo.Snapshot(ctx, testPlan, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	w.nodes++
	w.putPlan(int(snap.Revision), fmt.Sprintf("bump-%d", w.nodes), testsupport.NodeDoc(fmt.Sprintf("X%d", w.nodes), dag.NodeNonPR))
}

func (w *world) bind(id, role, kind, key, task string) {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(ctx, w.st, store.ScopeBindingsRow{BindingID: id, Role: role, ScopeKind: kind, ScopeKey: key, TaskID: task, HostID: "host", Status: "active", Revision: 1,
		CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// supervise gives the project the initiative lead the ordinary case has.
func (w *world) supervise(task string) {
	w.t.Helper()
	w.bind("bnd-lead-"+task, "supervisor", "initiative", testInitiative, task)
	if err := storeseed.InsertScopeLink(ctx, w.st, store.ScopeLinksRow{LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: testInitiative, UpperTaskID: task,
		LowerKind: "project", LowerKey: testProject, LowerTaskID: testOrch, Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// handover moves the initiative to a new lead: the old binding is archived, the new one bound and the link repointed.
func (w *world) handover(oldTask, newTask string) {
	w.t.Helper()
	if err := storeseed.ArchiveScopeBinding(ctx, w.st, "bnd-lead-"+oldTask, "archived", "bnd-lead-"+newTask, "t2"); err != nil {
		w.t.Fatal(err)
	}
	w.bind("bnd-lead-"+newTask, "supervisor", "initiative", testInitiative, newTask)
	if err := storeseed.RepointScopeLink(ctx, w.st, "lnk-project", "active", testOrch, newTask, "t2"); err != nil {
		w.t.Fatal(err)
	}
}

// seat registers a live store-scope supervisor binding.
func (w *world) seat(task string) {
	w.t.Helper()
	w.bind("bnd-seat-"+task, "supervisor", "store", "store", task)
}

func (w *world) writer() *Writer {
	return &Writer{Store: w.st, Rows: ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}, Resolver: StoreResolver{Store: w.st}, Sender: w.sender, ArtifactDir: w.t.TempDir()}
}

func (w *world) up(kind, milestone string, body string) UpResult {
	w.t.Helper()
	res, err := w.writer().Up(ctx, UpRequest{Kind: kind, Run: testRun, Milestone: milestone, Body: []byte(body)})
	if err != nil {
		w.t.Fatalf("report-up: %v", err)
	}
	return res
}

func (w *world) rows(kinds ...string) []Row {
	w.t.Helper()
	rows, err := ReadRows(ctx, w.st.DB, "", kinds...)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

// fakeSender stands in for crw manage send-parent: it records what it was asked and answers what the test says.
type fakeSender struct {
	mu         sync.Mutex
	supervisor []SupervisorSend
	parent     []ParentSend
	// replySupervisor and replyParent answer; the defaults accept a supervisor notice and queue a parent notice.
	replySupervisor func(SupervisorSend) (SendResult, error)
	replyParent     func(ParentSend) (SendResult, error)
	// onSupervisor runs when a supervisor notice arrives, before it is answered.
	onSupervisor func(SupervisorSend)
}

func (f *fakeSender) SendSupervisor(_ context.Context, s SupervisorSend) (SendResult, error) {
	f.mu.Lock()
	f.supervisor = append(f.supervisor, s)
	hook, reply := f.onSupervisor, f.replySupervisor
	f.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	if reply != nil {
		return reply(s)
	}
	return SendResult{Class: "accepted", RequestID: "req-" + s.LogicalID}, nil
}

func (f *fakeSender) SendParent(_ context.Context, p ParentSend) (SendResult, error) {
	f.mu.Lock()
	f.parent = append(f.parent, p)
	reply := f.replyParent
	f.mu.Unlock()
	if reply != nil {
		return reply(p)
	}
	class := "queued"
	if !p.Queue {
		class = "accepted"
	}
	return SendResult{Class: class, RequestID: "req-" + p.LogicalID}, nil
}

func (f *fakeSender) supervisorSent() []SupervisorSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SupervisorSend(nil), f.supervisor...)
}

func (f *fakeSender) parentSent() []ParentSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ParentSend(nil), f.parent...)
}

func readBody(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var _ = strings.TrimSpace
var _ = engine.KindFlow
