package linkage

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Part A2: test_linkage_peer.py, test_linkage_queries.py, test_linkage_recovery.py.
// Every scenario is replayed against testdata/python_a2.json (gen_a2.py, live Python).

const (
	thirdProject = "PROJ-3"
	thirdParent  = "01parent-three"
)

var pythonA2 = pythonFile("testdata/python_a2.json")

// sameAsA2 is sameAsPython against python_a2.json.
func (w *world) sameAsA2(name string) []map[string]any {
	w.t.Helper()
	return w.sameAs(pythonA2, name)
}

func (w *world) twoProjects() {
	w.t.Helper()
	w.superviseDefault()
	s := w.supervision()
	s.project, s.parent = otherProject, parentEP(otherParent)
	w.must(w.supervise(s))
}

func (w *world) peer(left, leftTask, right, rightTask string) any {
	return w.step(w.r.RegisterPeer(w.ctx, left, parentEP(leftTask), right, parentEP(rightTask)))
}

func (w *world) peered() {
	w.t.Helper()
	w.twoProjects()
	w.must(w.r.RegisterPeer(w.ctx, project, parentEP(parent), otherProject, parentEP(otherParent)))
}

func (w *world) hand(key, expect, to, evidence string) {
	w.t.Helper()
	w.must(w.r.Handover(w.ctx, "parent", key, expect, parentEP(to), nil, evidence, "test"))
}

func (w *world) executionEdges() {
	w.rows("SELECT link_id, lower_key, lower_task_id FROM scope_links" +
		" WHERE link_kind = 'execution' AND status IN ('active','paused') ORDER BY link_id")
}

func (w *world) down(kind, key string) contract.OrderedObject {
	answer := w.r.Down(w.ctx, kind, key)
	w.step(answer, nil)
	return answer
}

func (w *world) up(sel registry.UpSelector) contract.OrderedObject {
	answer := w.r.Up(w.ctx, sel)
	w.step(answer, nil)
	return answer
}

func (w *world) linkStep(lid string) {
	link, err := w.r.Link(w.ctx, lid)
	if link == nil {
		w.step(nil, err)
		return
	}
	w.step(link, err)
}

func (w *world) bindingStep(bid string) {
	b, err := w.r.Binding(w.ctx, bid)
	if b == nil {
		w.step(nil, err)
		return
	}
	w.step(b, err)
}

// unreadable records an unreadable answer without its driver-specific detail, after checking
// that the detail is there, and drops Python's detail at the same step index.
func (w *world) unreadable(answer contract.OrderedObject, python []map[string]any) {
	w.t.Helper()
	if field(answer, "state") != "unreadable" || field(answer, "readable") != false || text(field(answer, "detail")) == "" {
		w.t.Fatalf("unreadable answer %v", answer)
	}
	if ok, _ := python[len(w.steps)]["ok"].(map[string]any); ok != nil {
		delete(ok, "detail")
	}
	w.step(dropDetail(answer), nil)
}

func (w *world) a2(name string) []map[string]any {
	w.t.Helper()
	all, err := pythonA2()
	if err != nil {
		w.t.Fatal(err)
	}
	return all[name]
}

// Test26_LPR1: a peer link is one record whichever side registers it and across a handover.
func Test26_LPR1_a_peer_link_is_one_record(t *testing.T) {
	w := newWorld(t)
	w.twoProjects()
	first := w.peer(project, parent, otherProject, otherParent).(contract.OrderedObject)
	mirrored := w.peer(otherProject, otherParent, project, parent).(contract.OrderedObject)
	w.rows("SELECT link_id FROM scope_links WHERE link_kind = 'peer'")
	w.hand(otherProject, otherParent, thirdParent, "the peer project changed hands")
	again := w.peer(project, parent, otherProject, thirdParent).(contract.OrderedObject)
	peers := w.rows("SELECT link_id FROM scope_links WHERE link_kind = 'peer'")
	w.sameAsA2("lpr1_one_record")
	if field(first, "linkId") != field(mirrored, "linkId") || field(again, "linkId") != field(first, "linkId") || len(peers) != 1 {
		t.Fatalf("peer link ids %v %v %v, %d rows", field(first, "linkId"), field(mirrored, "linkId"), field(again, "linkId"), len(peers))
	}
}

// Test26_LPR2: a peer link adds no hierarchy level; each project keeps one execution owner.
func Test26_LPR2_a_peer_link_adds_no_level(t *testing.T) {
	w := newWorld(t)
	w.twoProjects()
	w.executionEdges()
	downBefore := w.down("initiative", initiative)
	upBefore := w.up(registry.UpSelector{Task: ns(parent)})
	w.peer(project, parent, otherProject, otherParent)
	w.executionEdges()
	downAfter := w.down("initiative", initiative)
	upAfter := w.up(registry.UpSelector{Task: ns(parent)})
	for _, p := range []string{project, otherProject} {
		w.rows("SELECT task_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ?"+
			"  AND role = ? AND status IN ('active','paused')", "project", p, "parent")
	}
	want := w.sameAsA2("lpr2_no_level")
	if len(field(downBefore, "levels").([]any)) == 0 {
		t.Fatal("the fixture produced no hierarchy to compare")
	}
	if !sameJSON(t, downBefore, downAfter) || !sameJSON(t, upBefore, upAfter) || !sameJSON(t, want[0], want[4]) {
		t.Fatal("the peer link changed the walk or the execution edges")
	}
}

// Test26_LPR3: peer refusals (scope_cycle, scope_role_mismatch twice).
func Test26_LPR3_peer_refusals(t *testing.T) {
	w := newWorld(t)
	w.twoProjects()
	w.peer(project, parent, project, parent)
	w.peer(project, parent, otherProject, thirdParent)
	w.peer(project, parent, thirdProject, thirdParent)
	want := w.sameAsA2("lpr3_refusals")
	for i, reason := range []string{"scope_cycle", "scope_role_mismatch", "scope_role_mismatch"} {
		if got := want[i]["refused"].(map[string]any)["reason"]; got != reason {
			t.Fatalf("step %d: %v", i, got)
		}
	}
}

// Test26_LPR4: a linked counterpart answer carries the link revision and the real counterpart.
func Test26_LPR4_a_linked_counterpart_answer(t *testing.T) {
	w := newWorld(t)
	w.peered()
	w.counterpart(parent, otherParent, registry.CounterpartQuery{})
	want := w.sameAsA2("lpr4_linked")
	answer := want[0]["ok"].(map[string]any)
	link := answer["link"].(map[string]any)
	counterpart := answer["counterpart"].(map[string]any)
	if answer["state"] != "linked" || link["kind"] != "peer" || link["revision"] != 1.0 ||
		counterpart["taskId"] != otherParent || counterpart["hostId"] != host || counterpart["scopeKey"] != otherProject ||
		len(answer["findings"].([]any)) != 0 {
		t.Fatalf("answer %v", answer)
	}
}

// Test26_LPR5: counterpart findings words.
func Test26_LPR5_counterpart_findings(t *testing.T) {
	has := func(t *testing.T, step map[string]any, word string) bool {
		t.Helper()
		for _, f := range step["ok"].(map[string]any)["findings"].([]any) {
			if f == word {
				return true
			}
		}
		return false
	}
	t.Run("recipient_replaced", func(t *testing.T) {
		w := newWorld(t)
		w.peered()
		w.hand(otherProject, otherParent, thirdParent, "the peer project changed hands")
		w.counterpart(parent, thirdParent, registry.CounterpartQuery{QuotedRevision: sql.NullInt64{Int64: 1, Valid: true}})
		w.counterpart(parent, otherParent, registry.CounterpartQuery{})
		want := w.sameAsA2("lpr5_recipient_replaced")
		stale := want[0]["ok"].(map[string]any)
		owner := want[1]["ok"].(map[string]any)["currentOwner"].(map[string]any)
		if !has(t, want[0], "stale_revision") || stale["link"].(map[string]any)["revision"].(float64) <= 1 ||
			!has(t, want[1], "stale_owner") || owner["taskId"] != thirdParent {
			t.Fatalf("answers %v", want)
		}
	})
	t.Run("other_scope_and_roles", func(t *testing.T) {
		w := newWorld(t)
		w.peered()
		w.counterpart(parent, otherParent, registry.CounterpartQuery{QuotedScope: ns("PROJ-NOPE")})
		w.counterpart(supervisorTask, parent, registry.CounterpartQuery{})
		rid := w.register()
		w.must(w.r.AttachIssue(w.ctx, rid, project))
		w.counterpart(supervisorTask, child, registry.CounterpartQuery{})
		want := w.sameAsA2("lpr5_other_scope_and_roles")
		if !has(t, want[0], "foreign_scope") || want[1]["ok"].(map[string]any)["state"] != "linked" ||
			has(t, want[1], "wrong_role") || !has(t, want[2], "wrong_role") {
			t.Fatalf("answers %v", want)
		}
	})
	t.Run("sender_replaced", func(t *testing.T) {
		w := newWorld(t)
		w.peered()
		w.hand(project, parent, thirdParent, "the sending project changed hands")
		w.counterpart(parent, otherParent, registry.CounterpartQuery{})
		if want := w.sameAsA2("lpr5_sender_replaced"); !has(t, want[0], "stale_sender") {
			t.Fatalf("answer %v", want[0])
		}
	})
}

// Test26_LPR6: an unregistered task is unlinked; an unreadable store is unreadable.
func Test26_LPR6_unregistered_is_not_unreadable(t *testing.T) {
	w := newWorld(t)
	want := w.a2("lpr6_unregistered_and_unreadable")
	w.peered()
	w.counterpart(parent, "01nobody-at-all", registry.CounterpartQuery{})
	if err := w.s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	w.unreadable(w.r.Counterpart(w.ctx, parent, otherParent, registry.CounterpartQuery{}), want)
	w.sameAsA2("lpr6_unregistered_and_unreadable")
	first := want[0]["ok"].(map[string]any)
	last := want[1]["ok"].(map[string]any)
	if first["state"] != "unlinked" || first["readable"] != true || first["findings"].([]any)[0] != "unregistered_link" ||
		last["readable"] != false || len(last["findings"].([]any)) != 0 || last["link"] != nil {
		t.Fatalf("answers %v", want)
	}
}

// Test26_LQY1: three-level walks down and up, each level with its real task and host.
func Test26_LQY1_three_level_walks(t *testing.T) {
	w := newWorld(t)
	rid := w.scoped()
	down := w.down("initiative", initiative)
	w.up(registry.UpSelector{Task: ns(child)})
	w.up(registry.UpSelector{Relationship: ns(rid)})
	want := w.sameAsA2("lqy1_three_levels")
	var tasks []any
	for _, level := range field(down, "levels").([]any) {
		owner := field(level.(contract.OrderedObject), "owner").(contract.OrderedObject)
		tasks = append(tasks, field(owner, "taskId"))
		if text(field(owner, "hostId")) == "" {
			t.Fatal("a level reported no host identifier")
		}
	}
	if field(down, "state") != "resolved" || len(field(down, "gaps").([]any)) != 0 ||
		!sameJSON(t, tasks, []any{supervisorTask, parent, child}) {
		t.Fatalf("down %v", down)
	}
	for _, step := range want[1:] {
		var keys []any
		for _, level := range step["ok"].(map[string]any)["levels"].([]any) {
			keys = append(keys, level.(map[string]any)["scopeKey"])
		}
		if !sameJSON(t, keys, []any{issue, project, initiative}) {
			t.Fatalf("up levels %v", keys)
		}
	}
}

// Test26_LQY2: an unreadable store walks as unreadable with no levels or gaps.
func Test26_LQY2_an_unreadable_store_walks_as_unreadable(t *testing.T) {
	w := newWorld(t)
	want := w.a2("lqy2_unreadable")
	w.scoped()
	if err := w.s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	w.unreadable(w.r.Down(w.ctx, "initiative", initiative), want)
	w.unreadable(w.r.Up(w.ctx, registry.UpSelector{Task: ns(child)}), want)
	w.sameAsA2("lqy2_unreadable")
	for _, step := range want {
		answer := step["ok"].(map[string]any)
		if len(answer["levels"].([]any)) != 0 || len(answer["gaps"].([]any)) != 0 {
			t.Fatalf("answer %v", answer)
		}
	}
}

func gapWords(answer map[string]any) []any {
	var out []any
	for _, g := range answer["gaps"].([]any) {
		out = append(out, g.(map[string]any)["gap"])
	}
	return out
}

// Test26_LQY3: gap words unscoped_assignment, project_without_parent, no_supervisor.
func Test26_LQY3_gap_words(t *testing.T) {
	t.Run("unscoped_assignment", func(t *testing.T) {
		w := newWorld(t)
		w.register()
		w.up(registry.UpSelector{Issue: ns(issue)})
		answer := w.sameAsA2("lqy3_unscoped_assignment")[0]["ok"].(map[string]any)
		if answer["state"] != "unregistered" || answer["readable"] != true || !sameJSON(t, gapWords(answer), []any{"unscoped_assignment"}) {
			t.Fatalf("answer %v", answer)
		}
	})
	t.Run("project_without_parent", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.hand(project, parent, otherParent, "handing over")
		w.exec("UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", project)
		w.down("initiative", initiative)
		answer := w.sameAsA2("lqy3_project_without_parent")[0]["ok"].(map[string]any)
		if answer["state"] != "resolved" || !sameJSON(t, gapWords(answer), []any{"project_without_parent"}) {
			t.Fatalf("answer %v", answer)
		}
	})
	t.Run("no_supervisor", func(t *testing.T) {
		w := newWorld(t)
		w.must(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
		w.up(registry.UpSelector{Task: ns(parent)})
		answer := w.sameAsA2("lqy3_no_supervisor")[0]["ok"].(map[string]any)
		if !sameJSON(t, gapWords(answer), []any{"no_supervisor"}) {
			t.Fatalf("answer %v", answer)
		}
	})
}

// Test26_LQY4: contention rows for a recorded conflict, an instruction pair and owner drift.
func Test26_LQY4_contention_rows(t *testing.T) {
	contention := func(step map[string]any) []any { return step["ok"].(map[string]any)["contention"].([]any) }
	t.Run("recorded_conflict", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := w.supervision()
		s.initiative, s.supervisor = "INIT-9", supervisorEP("01supervisor-nine")
		w.step(w.supervise(s))
		w.down("project", project)
		want := w.sameAsA2("lqy4_recorded_conflict")
		rows := contention(want[1])
		if len(rows) == 0 || rows[0].(map[string]any)["reason"] != "duplicate_scope_owner" {
			t.Fatalf("contention %v", rows)
		}
	})
	t.Run("instruction_pair", func(t *testing.T) {
		w := newWorld(t)
		execution := w.superviseDefault()
		s := w.supervision()
		s.initiative, s.supervisor, s.kind = "INIT-2", supervisorEP("01supervisor-two"), "reference"
		w.step(w.supervise(s))
		for _, digest := range []string{"d-one", "d-two"} {
			w.record(initiative, supervisorTask, text(field(execution, "linkId")), digest, "project", project)
		}
		w.down("project", project)
		want := w.sameAsA2("lqy4_instruction_pair")
		found := false
		for _, row := range contention(want[3]) {
			found = found || row.(map[string]any)["contention"] == "instruction_conflict"
		}
		if !found {
			t.Fatal("no instruction_conflict")
		}
	})
	t.Run("owner_drift", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.exec("UPDATE scope_bindings SET task_id = ? WHERE scope_key = ? AND role = ?", otherParent, project, "parent")
		w.down("initiative", initiative)
		rows := contention(w.sameAsA2("lqy4_owner_drift")[0])
		if len(rows) != 1 || rows[0].(map[string]any)["recorded"] != parent || rows[0].(map[string]any)["live"] != otherParent {
			t.Fatalf("contention %v", rows)
		}
	})
}

var projectKeys = []string{"projectKey", "projectParentTaskId", "scopeState", "parentOwnsProject", "responsibleChild", "responsibleRelationship"}

// pickPresent is gen_a2's {k: record.get(k, "<absent>")}.
func pickPresent(record contract.OrderedObject, keys []string) contract.OrderedObject {
	out := contract.OrderedObject{}
	for _, key := range keys {
		var value any = "<absent>"
		for _, f := range record {
			if f.Key == key {
				value = f.Value
			}
		}
		out = append(out, contract.Field{Key: key, Value: value})
	}
	return out
}

func (w *world) forIssue() contract.OrderedObject {
	w.t.Helper()
	record, err := registry.NewAssignmentView(w.r).ForIssue(w.ctx, issue)
	if err != nil {
		w.t.Fatal(err)
	}
	w.step(pickPresent(record, projectKeys), nil)
	return record
}

// Test26_LQY5: the assignment view's project context: scoped, unscoped, unreadable.
func Test26_LQY5_assignment_view_project_context(t *testing.T) {
	w := newWorld(t)
	w.scoped()
	w.forIssue()
	// The project lookup fails while the rest of the view reads (Python closes the connection
	// under _project_context; dropping its table reaches the same except branch through for_issue).
	w.exec("DROP TABLE relationship_scope")
	w.forIssue()
	want := w.sameAsA2("lqy5_project_context")
	if want[0]["ok"].(map[string]any)["scopeState"] != "scoped" || want[1]["ok"].(map[string]any)["scopeState"] != "unreadable" {
		t.Fatalf("scope states %v", want)
	}

	u := newWorld(t)
	u.register()
	u.forIssue()
	got := u.sameAsA2("lqy5_unscoped")[0]["ok"].(map[string]any)
	if got["projectKey"] != nil || got["scopeState"] != "unscoped" || got["responsibleChild"] != child {
		t.Fatalf("unscoped %v", got)
	}
}

var newTables = []string{"scope_bindings", "scope_links", "relationship_scope", "scope_directives", "linkage_conflicts"}

var legacyRows = []string{
	"INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome," +
		" producer, turn_thread_id, turn_id, turn_status, receipt, path_binding_mode, stage," +
		" first_seen_at, last_seen_at) VALUES ('evt-legacy', ?, 1, 'sha256:abc', 'ready_for_review'," +
		" 'child', '01child-task', 'turn-dispatch-1', 'completed', '{}', NULL, 'accepted', 't0', 't0')",
	"INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id," +
		" state, attempt_count, hold_reason, created_at, updated_at) VALUES ('evt-legacy', ?," +
		" 'completion', '01parent-task', '01parent-task', 'in_flight', 1, NULL, 't0', 't0')",
	"INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, record," +
		" sealed, observed_at) VALUES ('req-legacy', 'evt-legacy', 1, 'completion', 'transmitting'," +
		" NULL, NULL, 0, 't0')",
}

var snapshots = []string{
	"SELECT relationship_id, issue_key, status, parent_task_id, child_task_id, execution_generation" +
		" FROM relationships ORDER BY rowid",
	"SELECT relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id" +
		" FROM generations ORDER BY rowid",
	"SELECT event_id, relationship_id, revision_hash, outcome, stage FROM events ORDER BY rowid",
	"SELECT event_id, relationship_id, kind, recipient_task_id, state, attempt_count, hold_reason" +
		" FROM deliveries ORDER BY rowid",
	"SELECT request_id, event_id, attempt_no, kind, internal_state, state, sealed FROM attempts" +
		" ORDER BY rowid",
}

// reopenWithoutNewTables drops the linkage tables and reopens the store, as a pre-linkage
// store takes the schema: the DDL runs again over what it had.
func (w *world) reopenWithoutNewTables() {
	w.t.Helper()
	for _, table := range newTables {
		w.exec("DROP TABLE IF EXISTS " + table)
	}
	if err := w.s.Close(); err != nil {
		w.t.Fatal(err)
	}
	s, err := store.Open(context.Background(), w.path, "")
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = s.Close() })
	w.s = s
	w.r = &registry.Registry{Store: s, Now: w.r.Now, Policy: w.r.Policy}
}

// Test26_LRC1: an existing two-level store keeps every row across the linkage schema, and a
// pre-scoping assignment can be attached afterwards.
func Test26_LRC1_an_existing_store_keeps_what_it_had(t *testing.T) {
	w := newWorld(t)
	rid := w.register()
	if err := w.s.Transaction(w.ctx, func(ctx context.Context, conn *sql.Conn) error {
		for _, q := range legacyRows {
			if _, err := conn.ExecContext(ctx, q, rid); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range snapshots {
		w.rows(q)
	}
	w.reopenWithoutNewTables()
	for _, q := range snapshots {
		w.rows(q)
	}
	w.rows("SELECT name FROM sqlite_master WHERE type = 'table' AND name IN " +
		"('scope_bindings','scope_links','relationship_scope','scope_directives','linkage_conflicts') ORDER BY name")
	w.superviseDefault()
	w.step(w.r.AttachIssue(w.ctx, rid, project))
	w.ownerStep("issue", issue)
	want := w.sameAsA2("lrc1_existing_store")
	for i := range snapshots {
		if !sameJSON(t, want[i], want[i+len(snapshots)]) || len(want[i]["ok"].([]any)) == 0 {
			t.Fatalf("snapshot %d changed or empty", i)
		}
	}
}

func pickRegistered(x registry.Relationship) contract.OrderedObject {
	full := x.FullRecord()
	out := contract.OrderedObject{}
	for _, key := range []string{"relationshipId", "executionGeneration", "status", "issueKey"} {
		out = append(out, contract.Field{Key: key, Value: field(full, key)})
	}
	return out
}

func (w *world) again(projectKey string) (registry.Relationship, error) {
	return w.r.Register(w.ctx, registry.Registration{
		Parent: parentEP(parent), Child: registry.Endpoint{TaskID: child, HostID: host, Cwd: ns(root), CXCSession: ns("cxc-child")},
		IssueKey: issue, ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
		DispatchRequestID: "dispatch-1", DispatchTurnID: ns("turn-dispatch-1"), ProjectKey: projectKey})
}

// Test26_LRC2: registering with a project writes the lower level; re-registering never
// reinserts, records a project for an unscoped relationship, and refuses another project.
func Test26_LRC2_registering_with_a_project(t *testing.T) {
	t.Run("register_with_project", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		x, err := w.r.Register(w.ctx, registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")},
			Child: bare(child), IssueKey: "REL-NEW", ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
			DispatchRequestID: "dispatch-new", DispatchTurnID: ns("turn-new"), ProjectKey: project})
		w.step(x.ID, err)
		w.rows("SELECT project_key FROM relationship_scope WHERE relationship_id = ?", x.ID)
		w.ownerStep("issue", "REL-NEW")
		w.linkStep(registry.LinkID("execution", "project", project, "issue", "REL-NEW"))
		want := w.sameAsA2("lrc2_register_with_project")
		if want[3]["ok"] == nil {
			t.Fatal("no project to issue edge")
		}
	})
	t.Run("reregister", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		w.step(w.r.Attachment(w.ctx, rid))
		x, err := w.again(project)
		w.step(pickRegistered(x), err)
		got, err := w.r.Get(w.ctx, x.ID)
		w.step(len(got.Generations), err)
		w.ownerStep("issue", issue)
		w.step(w.r.Attachment(w.ctx, rid))
		_, err = w.again(otherProject)
		w.step(nil, err)
		want := w.sameAsA2("lrc2_reregister")
		if x.ID != rid || x.Generation != 1 || len(got.Generations) != 1 ||
			want[4]["ok"].(map[string]any)["projectKey"] != project || reasonOf(err) != "relationship_conflict" {
			t.Fatalf("re-register %v %v", x.ID, err)
		}
	})
}

func (w *world) issueState() {
	w.bindingStep(registry.BindingID("child", "issue", issue, child))
	w.linkStep(registry.LinkID("execution", "project", project, "issue", issue))
	w.ownerStep("issue", issue)
}

// Test26_LRC3: lifecycle propagation to the lower level.
func Test26_LRC3_lifecycle_propagation(t *testing.T) {
	status := func(step map[string]any) any { return step["ok"].(map[string]any)["status"] }
	t.Run("archive", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.issueState()
		want := w.sameAsA2("lrc3_archive")
		if status(want[0]) != "archived" || status(want[1]) != "archived" || want[2]["ok"] != nil {
			t.Fatalf("archive %v", want)
		}
	})
	t.Run("resume", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.ownerStep("issue", issue)
		w.must(w.resume(rid))
		w.issueState()
		want := w.sameAsA2("lrc3_resume")
		if want[0]["ok"] != nil || status(want[2]) != "active" || want[3]["ok"].(map[string]any)["taskId"] != child {
			t.Fatalf("resume %v", want)
		}
	})
	t.Run("unscoped", func(t *testing.T) {
		w := newWorld(t)
		rid := w.register()
		w.setStatus(rid, "archived")
		w.rows("SELECT * FROM scope_bindings")
		w.rows("SELECT * FROM scope_links")
		w.sameAsA2("lrc3_unscoped")
	})
	t.Run("replacement", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		x, err := w.r.Register(w.ctx, registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")},
			Child: bare("01child-two"), IssueKey: issue, ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
			DispatchRequestID: "dispatch-replacement", DispatchTurnID: ns("turn-replacement"), Supersedes: original, ProjectKey: project})
		if err != nil {
			t.Fatal(err)
		}
		w.statusOf(original)
		w.ownerStep("issue", issue)
		w.linkStep(registry.LinkID("execution", "project", project, "issue", issue))
		w.step(w.r.Attachment(w.ctx, x.ID))
		want := w.sameAsA2("lrc3_replacement")
		edge := want[2]["ok"].(map[string]any)
		if want[0]["ok"] != "archived" || edge["status"] != "active" || edge["lower"].(map[string]any)["taskId"] != "01child-two" ||
			edge["revision"].(float64) <= 1 || want[3]["ok"].(map[string]any)["projectKey"] != project {
			t.Fatalf("replacement %v", want)
		}
	})
}

var errInterrupted = errors.New("the writer stopped partway through")

// Test26_LRC4: nothing partial survives a refusal or an interrupted transition.
func Test26_LRC4_nothing_partial_survives(t *testing.T) {
	t.Run("refused_supervision", func(t *testing.T) {
		w := newWorld(t)
		w.must(w.r.BindScopeAs(w.ctx, "child", "ISS-OTHER", parentEP(otherParent), "active"))
		s := w.supervision()
		s.parent = parentEP(otherParent)
		w.step(w.supervise(s))
		w.ownerStep("initiative", initiative)
		w.rows("SELECT scope_key FROM scope_bindings ORDER BY scope_key")
		w.rows("SELECT link_id FROM scope_links")
		conflicts := w.conflicts()
		want := w.sameAsA2("lrc4_refused_supervision")
		if want[0]["refused"].(map[string]any)["reason"] != "scope_role_mismatch" || len(conflicts) != 1 {
			t.Fatalf("refused supervision %v", want)
		}
	})
	t.Run("half_written", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		w.exec("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?)", rid, project, fakeISO)
		w.up(registry.UpSelector{Relationship: ns(rid)})
		answer := w.sameAsA2("lrc4_half_written")[0]["ok"].(map[string]any)
		if answer["state"] != "resolved" || !sameJSON(t, gapWords(answer), []any{"issue_without_child"}) {
			t.Fatalf("answer %v", answer)
		}
	})
	t.Run("failure_partway", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		// Compose joins the attachment's own transaction to this one, as attach_in writes in
		// the caller's; every read inside uses the body's ctx and sees the uncommitted rows.
		err := w.s.Compose(w.ctx, func(ctx context.Context, _ *sql.Conn) error {
			if _, err := w.r.AttachIssue(ctx, rid, project); err != nil {
				return err
			}
			w.step(nil, nil)
			rows, err := w.s.All(ctx, "SELECT relationship_id, project_key FROM relationship_scope WHERE relationship_id = ?", rid)
			if err != nil {
				return err
			}
			list := []any{}
			for _, row := range rows {
				o := contract.OrderedObject{}
				for _, c := range row {
					o = append(o, contract.Field{Key: c.Name, Value: c.Value})
				}
				list = append(list, o)
			}
			w.step(list, nil)
			return errInterrupted
		})
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("transaction %v", err)
		}
		w.rows("SELECT 1 FROM relationship_scope WHERE relationship_id = ?", rid)
		w.ownerStep("issue", issue)
		w.linkStep(registry.LinkID("execution", "project", project, "issue", issue))
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.ownerStep("issue", issue)
		want := w.sameAsA2("lrc4_failure_partway")
		if len(want[1]["ok"].([]any)) != 1 || len(want[2]["ok"].([]any)) != 0 || want[6]["ok"].(map[string]any)["taskId"] != child {
			t.Fatalf("failure partway %v", want)
		}
	})
}

// Test26_LRC5: two concurrent attachments of one issue settle as one project.
func Test26_LRC5_concurrent_attachment_settles_as_one_project(t *testing.T) {
	w := newWorld(t)
	w.superviseDefault()
	s := w.supervision()
	s.initiative, s.project, s.supervisor, s.parent = "INIT-2", otherProject, supervisorEP("01supervisor-two"), parentEP(otherParent)
	w.must(w.supervise(s))
	rid := w.register()
	attach := func(p string) func(*registry.Registry) (contract.OrderedObject, error) {
		return func(r *registry.Registry) (contract.OrderedObject, error) {
			_, err := r.AttachIssue(context.Background(), rid, p)
			return nil, err
		}
	}
	outcomes := contend(t, w.path, attach(project), attach(otherProject))
	wins, reasons := 0, []any{}
	for _, o := range outcomes {
		if o.err == nil {
			wins++
		} else if reason := reasonOf(o.err); reason != "" {
			reasons = append(reasons, reason)
		} else {
			t.Fatalf("unexpected error %v", o.err)
		}
	}
	rows := w.rowsQuiet("SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
	w.step(contract.OrderedObject{{Key: "rows", Value: len(rows)}, {Key: "wins", Value: wins}, {Key: "refusals", Value: reasons}}, nil)
	w.sameAsA2("lrc5_concurrent_attach")
}
