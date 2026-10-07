package supervisor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// review722World is the smallest store that can raise and stage one obligation: a relationship with
// its project owner and its child, and an event with the work report that makes it reportable. The
// tests below read the packet Channel.Show returns from a message that was really staged, because
// the substitution CRW-950 records is a fact about the stored packet.
type review722World struct {
	t          *testing.T
	s          *store.Store
	ctx        context.Context
	at         string
	project    string
	initiative string
	supervisor string
	seatTask   string
	event      string
}

func newReview722World(t *testing.T) *review722World {
	t.Helper()
	const (
		relationship = "rel-722"
		project      = "PRJ-722"
		issue        = "CRW-722"
		parent       = "01parent-722"
		child        = "01child-722"
		initiative   = "INI-722"
		supervisor   = "01supervisor-722"
		seat         = "01management-722"
		event        = "event-722"
		at           = "2023-11-14T22:13:20.000000+00:00"
	)
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w := &review722World{t: t, s: s, ctx: ctx, at: at, project: project, initiative: initiative,
		supervisor: supervisor, seatTask: seat, event: event}
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,'active',?,?,?,?,1,'[]','[]',?,?)`,
		relationship, issue, parent, "host", child, "host", at, at); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES (?,?,1,'abc123456789abcdef','ready_for_review','child','child','turn-722','completed','{}',?,?)`,
		event, relationship, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,1,?,1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge',?)`,
		event, relationship, at); err != nil {
		t.Fatal(err)
	}
	if err = storeseed.RecordRelationshipScope(ctx, s, relationship, project, "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{
		{BindingID: "bnd-722-child", Role: "child", ScopeKind: "issue", ScopeKey: issue, TaskID: child, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
		{BindingID: "bnd-722-parent", Role: "parent", ScopeKind: "project", ScopeKey: project, TaskID: parent, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
	} {
		if err = storeseed.InsertScopeBinding(ctx, s, b); err != nil {
			t.Fatal(err)
		}
	}
	if err = storeseed.InsertScopeLink(ctx, s, store.ScopeLinksRow{LinkID: "lnk-722-issue", LinkKind: "execution",
		UpperKind: "project", UpperKey: project, UpperTaskID: parent,
		LowerKind: "issue", LowerKey: issue, LowerTaskID: child,
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	return w
}

// seat registers the live store-scope supervisor: the one seat no Linear level owns (CRW-450).
func (w *review722World) seat() {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-722-store",
		Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: w.seatTask, HostID: "host",
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// superviseInitiative gives the project the initiative supervisor the ordinary case has.
func (w *review722World) superviseInitiative() {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-722-supervisor",
		Role: "supervisor", ScopeKind: "initiative", ScopeKey: w.initiative, TaskID: w.supervisor, HostID: "host",
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
	if err := storeseed.InsertScopeLink(w.ctx, w.s, store.ScopeLinksRow{LinkID: "lnk-722-project", LinkKind: "execution",
		UpperKind: "initiative", UpperKey: w.initiative, UpperTaskID: w.supervisor,
		LowerKind: "project", LowerKey: w.project, LowerTaskID: "01parent-722",
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// stage stages the event's obligation and returns the message id.
func (w *review722World) stage() string {
	w.t.Helper()
	c := &Channel{Store: w.s, Linkage: StoreLinkage{w.s}}
	o, err := c.FromEvent(w.ctx, w.event)
	if err != nil || o == nil {
		w.t.Fatalf("obligation %v: %v", o, err)
	}
	result, err := c.Stage(w.ctx, *o, "", w.at)
	if err != nil {
		w.t.Fatalf("stage: %v", err)
	}
	id, _ := result["messageId"].(string)
	if id == "" {
		w.t.Fatalf("stage returned no messageId: %v", result)
	}
	return id
}

// recipientOf is the recipient object of the packet Channel.Show returns for one staged message.
func (w *review722World) recipientOf(id string) map[string]any {
	w.t.Helper()
	c := &Channel{Store: w.s, Linkage: StoreLinkage{w.s}}
	shown, err := c.Show(w.ctx, id)
	if err != nil {
		w.t.Fatalf("show: %v", err)
	}
	packet, ok := shown["packet"].(map[string]any)
	if !ok {
		w.t.Fatalf("show returned no packet object: %v", shown["packet"])
	}
	envelope, ok := packet["envelope"].(map[string]any)
	if !ok {
		w.t.Fatalf("packet carries no envelope object: %v", packet)
	}
	recipient, ok := envelope["recipient"].(map[string]any)
	if !ok {
		w.t.Fatalf("envelope carries no recipient object: %v", envelope)
	}
	return recipient
}

// TestSupervisorReview722StoreFallbackIsRecorded: when no initiative supervises the project and the
// store-scope supervisor answers instead, the stored report says the recipient came from the store.
func TestSupervisorReview722StoreFallbackIsRecorded(t *testing.T) {
	t.Parallel()
	w := newReview722World(t)
	w.seat()
	recipient := w.recipientOf(w.stage())
	if recipient["scopeKind"] != "store" {
		t.Fatalf("a store-scope recipient is not recorded as coming from the store: recipient %v", recipient)
	}
	if recipient["taskId"] != w.seatTask {
		t.Fatalf("recipient task %v, want the store seat %q", recipient["taskId"], w.seatTask)
	}
}

// TestSupervisorReview722InitiativeRecipientIsRecorded: the ordinary case records the initiative,
// and the recipient task is exactly the one the report went to before this change.
func TestSupervisorReview722InitiativeRecipientIsRecorded(t *testing.T) {
	t.Parallel()
	w := newReview722World(t)
	w.superviseInitiative()
	w.seat()
	recipient := w.recipientOf(w.stage())
	if recipient["scopeKind"] != "initiative" {
		t.Fatalf("an initiative recipient is not recorded as coming from an initiative: recipient %v", recipient)
	}
	if recipient["taskId"] != w.supervisor {
		t.Fatalf("recipient task %v, want the initiative supervisor %q", recipient["taskId"], w.supervisor)
	}
}
