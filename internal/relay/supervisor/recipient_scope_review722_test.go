package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// review722World is the smallest store that can raise and stage one obligation: a relationship with
// its project owner and its child, and an event with the work report that makes it reportable. The
// tests below read the packet Channel.Show returns from a message that was really staged, because
// the substitution CRW-950 records is a fact about the stored packet.
type review722World struct {
	t            *testing.T
	s            *store.Store
	ctx          context.Context
	at           string
	relationship string
	project      string
	initiative   string
	supervisor   string
	seatTask     string
	event        string
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
		relationship: relationship, supervisor: supervisor, seatTask: seat, event: event}
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

// notice seeds one open fault of class report_omitted, anchored to the world's relationship, and
// one notification about it: the rows a raised notification has when the deliverer reads its facts.
func (w *review722World) notice(fault, notification string) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, `INSERT INTO fault_ledger (fault_id,product,fault_class,component,severity,signature,scope,scope_key,state,first_seen_at,last_seen_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		fault, "crw", "report_omitted", "component", "broken", `{"relationship": "`+w.relationship+`", "turn": "turn-1"}`, `{"projectKey": "`+w.project+`"}`, "P", "open", "t", "t", "t"); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.s.DB.ExecContext(w.ctx, `INSERT INTO fault_notifications (notification_id,fault_id,product,kind,cycle,state,lease_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		notification, fault, "crw", "blocking", 1, "pending", nil, "t", "t"); err != nil {
		w.t.Fatal(err)
	}
}

// stageNotice stages the notification's fault notice and returns its message id.
func (w *review722World) stageNotice(notification string) string {
	w.t.Helper()
	ledger := &faults.Ledger{Store: w.s, Clock: &delivery.FakeClock{T: 1700000000}}
	channel := NoticeChannel{Channel: &Channel{Store: w.s, Linkage: StoreLinkage{w.s}}, Ledger: ledger}
	notice, err := ledger.NoticeFacts(w.ctx, notification)
	if err != nil || notice == nil {
		w.t.Fatalf("notice facts %q: %v, %v", notification, notice, err)
	}
	answer, err := channel.StageNotice(w.ctx, notice)
	if err != nil {
		w.t.Fatalf("stage notice: %v", err)
	}
	id, _ := answer["messageId"].(string)
	if id == "" {
		w.t.Fatalf("stage notice returned no messageId: %v", answer)
	}
	return id
}

// stageNoticeForProject stages a fault notice whose fault is anchored to no relationship this store
// holds, so the notice is addressed to the project alone (the resolveNoticeProject path). It returns
// the message id, or the refusal when nothing was staged.
func (w *review722World) stageNoticeForProject(notification string) (string, error) {
	w.t.Helper()
	ledger := &faults.Ledger{Store: w.s, Clock: &delivery.FakeClock{T: 1700000000}}
	channel := NoticeChannel{Channel: &Channel{Store: w.s, Linkage: StoreLinkage{w.s}}, Ledger: ledger}
	notice, err := ledger.NoticeFacts(w.ctx, notification)
	if err != nil || notice == nil {
		w.t.Fatalf("notice facts %q: %v, %v", notification, notice, err)
	}
	notice["anchor"] = "project:" + w.project
	answer, err := channel.StageNotice(w.ctx, notice)
	if err != nil {
		return "", err
	}
	id, _ := answer["messageId"].(string)
	return id, nil
}

// TestSupervisorReview722StoreFallbackNoticeByProjectIsRecorded: a notice addressed to a project
// alone takes the same store-seat fallback as one addressed to a relationship. When no initiative
// supervises the project and the store seat answers, the resolution and the stored packet say so
// rather than refusing with nowhere to send.
func TestSupervisorReview722StoreFallbackNoticeByProjectIsRecorded(t *testing.T) {
	t.Parallel()
	w := newReview722World(t)
	w.seat()
	w.notice("abc123", "n-722")
	id, err := w.stageNoticeForProject("n-722")
	if err != nil {
		t.Fatalf("a project-only notice was refused although a store supervisor answers: %v", err)
	}
	recipient := w.recipientOf(id)
	if recipient["scopeKind"] != "store" {
		t.Fatalf("a project-only store-scope notice recipient is not recorded as coming from the store: recipient %v", recipient)
	}
	if recipient["taskId"] != w.seatTask {
		t.Fatalf("notice recipient task %v, want the store seat %q", recipient["taskId"], w.seatTask)
	}
}

// seatForInitiative binds the store seat to the same task that supervises the initiative, so a
// hierarchy change can move the recipient's seat without moving the recipient's task.
func (w *review722World) seatForInitiative() {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-722-store",
		Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: w.supervisor, HostID: "host",
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// TestSupervisorReview722NoticeRefusesScopeKindDrift: the recipient's scope kind is compared with
// the rest of the resolution under the write lock. The same task supervises the initiative and holds
// the store seat, so when the initiative level goes between the pre-lock read and the lock the
// recipient task does not move at all - only its seat does. The packet was composed from the
// pre-lock read, and committing it would freeze the stale kind into what supervisor-show returns.
func TestSupervisorReview722NoticeRefusesScopeKindDrift(t *testing.T) {
	t.Parallel()
	w := newReview722World(t)
	w.superviseInitiative()
	w.seatForInitiative()
	w.notice("abc123", "n-722")
	ledger := &faults.Ledger{Store: w.s, Clock: &delivery.FakeClock{T: 1700000000}}
	notice, err := ledger.NoticeFacts(w.ctx, "n-722")
	if err != nil || notice == nil {
		t.Fatalf("notice facts: %v, %v", notice, err)
	}
	// The linkage answers the initiative level on the first read and only the store seat on the
	// second, which is the interleaving a real hierarchy change can produce.
	base := StoreLinkage{w.s}
	call := 0
	linkage := &scriptedLinkage{inner: base, script: func(n int, reading map[string]any) (map[string]any, error) {
		call = n
		if n == 1 {
			return reading, nil
		}
		levels := make([]any, 0)
		for _, raw := range reading["levels"].([]any) {
			if level, ok := raw.(map[string]any); ok && level["scopeKind"] == "initiative" {
				continue
			}
			levels = append(levels, raw)
		}
		folded := map[string]any{}
		for key, value := range reading {
			folded[key] = value
		}
		folded["levels"] = levels
		return folded, nil
	}}
	channel := NoticeChannel{Channel: &Channel{Store: w.s, Linkage: linkage}, Ledger: ledger}
	_, err = channel.StageNotice(w.ctx, notice)
	if call < 2 {
		t.Fatalf("the notice was not read under the write lock (reads: %d)", call)
	}
	var noticeErr *faults.NoticeError
	if !errors.As(err, &noticeErr) || noticeErr.Kind != "relation_owner_drift" {
		t.Fatalf("a scope-only hierarchy change was not refused: %v", err)
	}
	var staged int
	if err = w.s.DB.QueryRowContext(w.ctx, "SELECT COUNT(*) FROM supervisor_messages").Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Fatalf("a message was staged from a resolution the lock had already contradicted: %d", staged)
	}
}

// TestSupervisorReview722StoreFallbackNoticeIsRecorded: a fault notification is stored as a
// relay-envelope/1 parent_to_supervisor packet too, so a notice that went to the store seat says so
// in the packet Channel.Show returns, exactly as a report does.
func TestSupervisorReview722StoreFallbackNoticeIsRecorded(t *testing.T) {
	t.Parallel()
	w := newReview722World(t)
	w.seat()
	w.notice("abc123", "n-722")
	recipient := w.recipientOf(w.stageNotice("n-722"))
	if recipient["scopeKind"] != "store" {
		t.Fatalf("a store-scope notice recipient is not recorded as coming from the store: recipient %v", recipient)
	}
	if recipient["taskId"] != w.seatTask {
		t.Fatalf("notice recipient task %v, want the store seat %q", recipient["taskId"], w.seatTask)
	}
}

// TestSupervisorReview722InitiativeNoticeIsRecorded: the ordinary notice records the initiative.
func TestSupervisorReview722InitiativeNoticeIsRecorded(t *testing.T) {
	t.Parallel()
	w := newReview722World(t)
	w.superviseInitiative()
	w.seat()
	w.notice("abc123", "n-722")
	recipient := w.recipientOf(w.stageNotice("n-722"))
	if recipient["scopeKind"] != "initiative" {
		t.Fatalf("an initiative notice recipient is not recorded as coming from an initiative: recipient %v", recipient)
	}
	if recipient["taskId"] != w.supervisor {
		t.Fatalf("notice recipient task %v, want the initiative supervisor %q", recipient["taskId"], w.supervisor)
	}
}

// preChangePacket rewrites a stored packet to the shape it had before recipient.scopeKind existed:
// the field removed, re-encoded exactly as the channel writes a packet. A row stored before this
// change is byte-for-byte one of these, so the restatement comparisons can be asked about it.
// stageNoticeForReview722 stages the notice world's notification without recording a golden: these
// tests ask the code a question, they do not capture an answer for the fixture.
func stageNoticeForReview722(t *testing.T, w *noticeWorld) string {
	t.Helper()
	answer, err := w.channel.StageNotice(w.ctx, w.facts(t, nsID))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := answer["messageId"].(string)
	if id == "" {
		t.Fatalf("stage notice returned no messageId: %v", answer)
	}
	return id
}

func preChangePacket(t *testing.T, packet string) string {
	t.Helper()
	decoded, err := pyjson.Loads(packet, pyjson.LoadOptions{Python: true, RangeErrors: true})
	if err != nil {
		t.Fatalf("stored packet is not readable: %v", err)
	}
	object, ok := evidence.Object(decoded)
	if !ok {
		t.Fatalf("stored packet is not an object: %v", packet)
	}
	envelope, _ := object.Lookup("envelope")
	region, ok := evidence.Object(envelope)
	if !ok {
		t.Fatalf("stored packet carries no envelope object: %v", packet)
	}
	recipient, _ := region.Lookup("recipient")
	person, ok := evidence.Object(recipient)
	if !ok {
		t.Fatalf("stored envelope carries no recipient object: %v", packet)
	}
	stripped := make(pyjson.Object, 0, len(person))
	for _, field := range person {
		if field.Key != "scopeKind" {
			stripped = append(stripped, field)
		}
	}
	region = region.Set("recipient", stripped)
	object = object.Set("envelope", region)
	return pyjson.Dumps(object, pyjson.Options{SortKeys: true, Unicode: true})
}

// restatedRows is how many supervisor_message_restated rows the store holds.
func restatedRows(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM journal WHERE kind='supervisor_message_restated'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSupervisorReview722PreChangeReportIsNotRestated: adding the field alone never restates a
// report stored before it existed. Its packet bytes, observedAt, updated_at and journal are as they
// were, so a queued report is claimed without a restatement it did not ask for.
func TestSupervisorReview722PreChangeReportIsNotRestated(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	before, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", preChangePacket(t, before.Packet), id); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.c.Resolve(f.ctx, row.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := f.c.refreshProposal(f.ctx, row, r, f.at, 0)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a report stored before the field existed was restated for the field alone")
	}
	after, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Packet != row.Packet {
		t.Fatalf("the packet changed without a restatement:\n before %s\n after  %s", row.Packet, after.Packet)
	}
	if after.UpdatedAt != row.UpdatedAt {
		t.Fatalf("updated_at moved without a restatement: %q -> %q", row.UpdatedAt, after.UpdatedAt)
	}
	if n := restatedRows(t, f.s); n != 0 {
		t.Fatalf("a restatement row was written for the field alone: %d", n)
	}
}

// TestSupervisorReview722MovedReportIsRestatedWithScopeKind: the control. A report whose statement
// moved is restated, and the packet it writes carries the field: the comparison drops scopeKind only
// when the stored packet predates it, and a restatement for something else still records it.
func TestSupervisorReview722MovedReportIsRestatedWithScopeKind(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	before, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", preChangePacket(t, before.Packet), id); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.ExecContext(f.ctx, "UPDATE work_reports SET submission_no=2 WHERE event_id=?", f.event); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.c.Resolve(f.ctx, row.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := f.c.refreshProposal(f.ctx, row, r, f.at, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a report whose statement moved was not restated")
	}
	after, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if recipient := recipientOfPacket(t, after.Packet); recipient["scopeKind"] != "initiative" {
		t.Fatalf("a restatement did not write the recipient's scope kind: recipient %v", recipient)
	}
	if n := restatedRows(t, f.s); n != 1 {
		t.Fatalf("restatement rows: %d, want 1", n)
	}
}

// recipientOfPacket is the recipient object of a stored packet string.
func recipientOfPacket(t *testing.T, packet string) map[string]any {
	t.Helper()
	decoded := evidence.Decode(packet)
	envelope, ok := evidence.Object(evidence.Item(decoded, "envelope"))
	if !ok {
		t.Fatalf("packet carries no envelope object: %v", packet)
	}
	recipient, _ := envelope.Lookup("recipient")
	person, ok := evidence.Object(recipient)
	if !ok {
		t.Fatalf("envelope carries no recipient object: %v", packet)
	}
	out := map[string]any{}
	for _, field := range person {
		out[field.Key] = field.Value
	}
	return out
}

// TestSupervisorReview722PreChangeNoticeIsNotRestated: the fault-notice path compares the same way,
// so a notice stored before the field existed is not restated either.
func TestSupervisorReview722PreChangeNoticeIsNotRestated(t *testing.T) {
	t.Parallel()
	w := newNoticeWorld(t)
	w.seed(t, nsSeed{signature: nsProjectSig, scope: nsProjectScope, state: "reserved", lease: nsNow + 300})
	w.exec(t, "INSERT INTO recipient_lifecycle (task_id, deliverable, observed_at) VALUES ('parent','yes','2023-11-14T22:13:10.000000+00:00')")
	id := stageNoticeForReview722(t, w)
	row, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.s.DB.ExecContext(w.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", preChangePacket(t, row.Packet), id); err != nil {
		t.Fatal(err)
	}
	row, err = w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	live, err := w.c.Resolve(w.ctx, row.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	restated, err := w.c.refreshNotice(w.ctx, row, live, nsAt, 0)
	if err != nil {
		t.Fatal(err)
	}
	if restated {
		t.Fatal("a notice stored before the field existed was restated for the field alone")
	}
	after, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Packet != row.Packet {
		t.Fatalf("the notice packet changed without a restatement:\n before %s\n after  %s", row.Packet, after.Packet)
	}
	if after.UpdatedAt != row.UpdatedAt {
		t.Fatalf("notice updated_at moved without a restatement: %q -> %q", row.UpdatedAt, after.UpdatedAt)
	}
	if n := restatedRows(t, w.s); n != 0 {
		t.Fatalf("a restatement row was written for the field alone: %d", n)
	}
}

// TestSupervisorReview722MovedNoticeIsRestatedWithScopeKind: the control for the notice path. A
// notice whose fault moved is restated, and its new packet carries the field.
func TestSupervisorReview722MovedNoticeIsRestatedWithScopeKind(t *testing.T) {
	t.Parallel()
	w := newNoticeWorld(t)
	w.seed(t, nsSeed{signature: nsProjectSig, scope: nsProjectScope, state: "reserved", lease: nsNow + 300})
	w.exec(t, "INSERT INTO recipient_lifecycle (task_id, deliverable, observed_at) VALUES ('parent','yes','2023-11-14T22:13:10.000000+00:00')")
	id := stageNoticeForReview722(t, w)
	row, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.s.DB.ExecContext(w.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", preChangePacket(t, row.Packet), id); err != nil {
		t.Fatal(err)
	}
	w.exec(t, "UPDATE fault_ledger SET severity='degraded' WHERE fault_id=?", nsFault)
	row, err = w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	live, err := w.c.Resolve(w.ctx, row.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	restated, err := w.c.refreshNotice(w.ctx, row, live, nsAt, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !restated {
		t.Fatal("a notice whose fault moved was not restated")
	}
	after, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if recipient := recipientOfPacket(t, after.Packet); recipient["scopeKind"] != "initiative" {
		t.Fatalf("a notice restatement did not write the recipient's scope kind: recipient %v", recipient)
	}
	if n := restatedRows(t, w.s); n != 1 {
		t.Fatalf("restatement rows: %d, want 1", n)
	}
}

// TestSupervisorReview722StageNoticeKeepsAPreChangeNotice: a notice stored before recipient.scopeKind
// existed is compared by StageNotice too, not only by refreshNotice. Staging the same notification
// again with nothing else moved neither rewrites the packet nor journals a restatement: the field
// alone is not a change to the notice.
func TestSupervisorReview722StageNoticeKeepsAPreChangeNotice(t *testing.T) {
	t.Parallel()
	w := newNoticeWorld(t)
	w.seed(t, nsSeed{signature: nsProjectSig, scope: nsProjectScope, state: "reserved", lease: nsNow + 300})
	w.exec(t, "INSERT INTO recipient_lifecycle (task_id, deliverable, observed_at) VALUES ('parent','yes','2023-11-14T22:13:10.000000+00:00')")
	id := stageNoticeForReview722(t, w)
	row, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.s.DB.ExecContext(w.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", preChangePacket(t, row.Packet), id); err != nil {
		t.Fatal(err)
	}
	row, err = w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := w.channel.StageNotice(w.ctx, w.facts(t, nsID))
	if err != nil {
		t.Fatal(err)
	}
	if answer["restated"] == true {
		t.Fatalf("staging a pre-change notice restated it for the field alone: %v", answer)
	}
	after, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Packet != row.Packet {
		t.Fatalf("the notice packet was rewritten without a restatement:\n before %s\n after  %s", row.Packet, after.Packet)
	}
	if after.UpdatedAt != row.UpdatedAt {
		t.Fatalf("notice updated_at moved without a restatement: %q -> %q", row.UpdatedAt, after.UpdatedAt)
	}
	if n := restatedRows(t, w.s); n != 0 {
		t.Fatalf("a restatement row was written for the field alone: %d", n)
	}
}

// TestSupervisorReview722EventlessOmissionRefreshesItsRecipientScopeKind: an omission carries no
// event, so its packet is composed from its frozen reading and the live resolution. The same task
// can change seat (the store seat answering instead of an initiative's supervisor) without the
// recipient task moving, and a claim or a transport start must not send the stale kind.
func TestSupervisorReview722EventlessOmissionRefreshesItsRecipientScopeKind(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	// A turn the fixture holds no final event for, so this omission is not superseded by one.
	reading := omissionReading24(f)
	reading["selectors"].(map[string]any)["turn"] = "turn-9"
	o := ObservationObligation(reading)
	if o == nil {
		t.Fatal("the omission reading raised no obligation")
	}
	result, err := f.c.StageWithReading(f.ctx, *o, reading, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	id := result["messageId"].(string)
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.EventID.Valid {
		t.Fatalf("the omission was staged with an event: %v", row.EventID)
	}
	if recipient := recipientOfPacket(t, row.Packet); recipient["scopeKind"] != "initiative" {
		t.Fatalf("the omission was staged without the initiative kind: recipient %v", recipient)
	}
	// The same task takes the store seat and the initiative level goes, so the recipient task does
	// not move while the seat it answers from does.
	if err = storeseed.InsertScopeBinding(f.ctx, f.s, store.ScopeBindingsRow{BindingID: "bnd-review722-store",
		Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: "supervisor", HostID: "host",
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.ExecContext(f.ctx, "UPDATE scope_bindings SET status='released' WHERE scope_kind='initiative'"); err != nil {
		t.Fatal(err)
	}
	row, err = f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.c.Resolve(f.ctx, row.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Recipient != row.RecipientTaskID {
		t.Fatalf("the fixture moved the recipient task: %q -> %q", row.RecipientTaskID, r.Recipient)
	}
	if r.RecipientScopeKind != "store" {
		t.Fatalf("the fixture did not move the seat: %q", r.RecipientScopeKind)
	}
	changed, err := f.c.refreshProposal(f.ctx, row, r, f.at, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an eventless omission was not restated when the seat its recipient answers from moved")
	}
	after, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if recipient := recipientOfPacket(t, after.Packet); recipient["scopeKind"] != "store" {
		t.Fatalf("the restated omission still names the old seat: recipient %v", recipient)
	}
}
