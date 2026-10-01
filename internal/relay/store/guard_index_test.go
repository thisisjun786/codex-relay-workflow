package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

// guardIndexes are store.py GUARD_INDEXES with the table each guards.
var guardIndexes = map[string]string{
	"scope_bindings_one_live_owner":       "scope_bindings",
	"scope_links_one_live_edge":           "scope_links",
	"merge_turns_one_live_holder":         "merge_turns",
	"merge_turns_one_live_claim":          "merge_turns",
	"execution_slots_one_live_subject":    "execution_slots",
	"edit_agreements_one_live_per_region": "edit_agreements",
}

// guardFixture is what the retired Python implementation left for one guard index (the fixture
// guard-<index>.json.gz): the rows of its store after its writer API was raced into a second live
// row from eight connections (every racer refused with a reason, queued or replayed; none reached
// the index), and the second live row it then wrote past the API, which raised
// sqlite3.IntegrityError 2067, bare and inside Store.transaction(), keeping nothing.
type guardFixture struct {
	Row   map[string]any `json:"row"`
	Store storeRows      `json:"store"`
}

// runGuard proves Go surfaces a write that reaches index as Python does (the golden).
func runGuard(t *testing.T, index string) {
	t.Helper()
	table := guardIndexes[index]
	var fixture guardFixture
	readFixture(t, "guard-"+strings.ReplaceAll(index, "_", "-")+".json", &fixture)
	// When: Go opens a store holding the rows Python's store held and writes the row Python forced
	// through the typed insert, bare and inside Transaction.
	s := restoreStore(t, filepath.Join(t.TempDir(), "restored", "relay.sqlite3"), fixture.Store)
	ctx := context.Background()
	insert := guardInsert(t, table, fixture.Row)
	bare := insert(ctx, s)
	transacted := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return insert(ctx, s) })
	// Then: the same surface. The driver's constraint error with the same extended code and the
	// same constraint text, never a refusal reason, and nothing written.
	var surfaced []any
	for _, err := range []error{bare, transacted} {
		var driverErr *sqlite.Error
		if !errors.As(err, &driverErr) || driverErr.Code() != 2067 || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".") || RefusalReason(err) != "" {
			t.Fatalf("Go second live row in %s: %v, want the IntegrityError 2067 of the index", index, err)
		}
		surfaced = append(surfaced, map[string]any{"code": driverErr.Code(), "error": err.Error()})
	}
	checkJSON(t, "second live row, bare and in a transaction", surfaced)
	var forced int
	key := guardKeyColumn(table)
	must(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+key+" = ?", fixture.Row[key]).Scan(&forced))
	if forced != 0 {
		t.Fatalf("Go kept the forced %s row", table)
	}
}

// guardKeyColumn is the primary key of each guarded table.
func guardKeyColumn(table string) string {
	return map[string]string{"scope_bindings": "binding_id", "scope_links": "link_id", "merge_turns": "turn_id",
		"execution_slots": "slot_id", "edit_agreements": "agreement_id"}[table]
}

// guardInsert is the typed Go insert for a guarded table, fed the row Python forced.
func guardInsert(t *testing.T, table string, row map[string]any) func(context.Context, *Store) error {
	t.Helper()
	r := pyRow(row)
	str := func(c string) sql.NullString {
		if v, ok := row[c].(string); ok {
			return sql.NullString{String: v, Valid: true}
		}
		return sql.NullString{}
	}
	num := func(c string) int64 { v, _ := row[c].(float64); return int64(v) }
	nullNum := func(c string) sql.NullInt64 {
		if v, ok := row[c].(float64); ok {
			return sql.NullInt64{Int64: int64(v), Valid: true}
		}
		return sql.NullInt64{}
	}
	switch table {
	case "scope_bindings":
		b := ScopeBindingsRow{BindingID: r.text("binding_id"), Role: r.text("role"), ScopeKind: r.text("scope_kind"), ScopeKey: r.text("scope_key"), TaskID: r.text("task_id"), HostID: r.text("host_id"), CWD: str("cwd"), CXCSession: str("cxc_session"), Status: r.text("status"), Revision: num("revision"), Supersedes: str("supersedes"), HandoverNote: str("handover_note"), CreatedAt: r.text("created_at"), UpdatedAt: r.text("updated_at")}
		return func(ctx context.Context, s *Store) error { return s.InsertScopeBinding(ctx, b) }
	case "scope_links":
		l := ScopeLinksRow{LinkID: r.text("link_id"), LinkKind: r.text("link_kind"), UpperKind: r.text("upper_kind"), UpperKey: r.text("upper_key"), UpperTaskID: r.text("upper_task_id"), LowerKind: r.text("lower_kind"), LowerKey: r.text("lower_key"), LowerTaskID: r.text("lower_task_id"), Status: r.text("status"), Revision: num("revision"), CreatedAt: r.text("created_at"), UpdatedAt: r.text("updated_at")}
		return func(ctx context.Context, s *Store) error { return s.InsertScopeLink(ctx, l) }
	case "merge_turns":
		m := MergeTurnsRow{TurnID: r.text("turn_id"), TargetKey: r.text("target_key"), Repository: r.text("repository"), BaseRef: r.text("base_ref"), ProjectKey: r.text("project_key"), HolderTaskID: r.text("holder_task_id"), HolderHostID: r.text("holder_host_id"), RelationshipID: str("relationship_id"), PRNumber: nullNum("pr_number"), CandidateHead: r.text("candidate_head"), DeclaredReady: num("declared_ready"), State: r.text("state"), Tenure: num("tenure"), RequestedAt: r.text("requested_at"), HeldAt: str("held_at"), UpdatedAt: r.text("updated_at")}
		return func(ctx context.Context, s *Store) error { return s.InsertMergeTurn(ctx, m) }
	case "execution_slots":
		e := ExecutionSlotsRow{SlotID: r.text("slot_id"), SubjectKind: r.text("subject_kind"), SubjectKey: r.text("subject_key"), ParentTaskID: r.text("parent_task_id"), ProjectKey: r.text("project_key"), InitiativeKey: str("initiative_key"), Tenure: num("tenure"), State: r.text("state"), ReservedBy: r.text("reserved_by"), ReservedAt: r.text("reserved_at"), Detail: str("detail")}
		return func(ctx context.Context, s *Store) error { return s.InsertExecutionSlot(ctx, e) }
	case "edit_agreements":
		a := EditAgreementsRow{AgreementID: r.text("agreement_id"), RegionID: r.text("region_id"), Repository: r.text("repository"), BaseRevision: r.text("base_revision"), LeftProject: r.text("left_project"), RightProject: r.text("right_project"), PeerLinkID: r.text("peer_link_id"), ProposerTaskID: r.text("proposer_task_id"), IssueKey: str("issue_key"), ConstraintText: r.text("constraint_text"), LeftCondition: str("left_condition"), RightCondition: str("right_condition"), LeftAcceptedAt: str("left_accepted_at"), RightAcceptedAt: str("right_accepted_at"), NextOwner: str("next_owner"), State: r.text("state"), Tenure: num("tenure"), Supersedes: str("supersedes"), ProposedAt: r.text("proposed_at"), UpdatedAt: r.text("updated_at")}
		return func(ctx context.Context, s *Store) error { return s.InsertEditAgreement(ctx, a) }
	}
	t.Fatalf("no guarded table %s", table)
	return nil
}

func TestGuard_scope_bindings_one_live_owner_fails_like_python(t *testing.T) {
	runGuard(t, "scope_bindings_one_live_owner")
}

func TestGuard_scope_links_one_live_edge_fails_like_python(t *testing.T) {
	runGuard(t, "scope_links_one_live_edge")
}

func TestGuard_merge_turns_one_live_holder_fails_like_python(t *testing.T) {
	runGuard(t, "merge_turns_one_live_holder")
}

func TestGuard_merge_turns_one_live_claim_fails_like_python(t *testing.T) {
	runGuard(t, "merge_turns_one_live_claim")
}

func TestGuard_execution_slots_one_live_subject_fails_like_python(t *testing.T) {
	runGuard(t, "execution_slots_one_live_subject")
}

func TestGuard_edit_agreements_one_live_per_region_fails_like_python(t *testing.T) {
	runGuard(t, "edit_agreements_one_live_per_region")
}

func liveBinding(id, task string) ScopeBindingsRow {
	return ScopeBindingsRow{BindingID: id, Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-A", TaskID: task, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}
}

func liveSlot(id, parent string) ExecutionSlotsRow {
	return ExecutionSlotsRow{SlotID: id, SubjectKind: "assignment", SubjectKey: "S1", ParentTaskID: parent, ProjectKey: "PRJ-A", Tenure: 1, State: "held", ReservedBy: parent, ReservedAt: "t"}
}

func TestGuard_released_rows_do_not_compete(t *testing.T) {
	// Given: a slot released, and an archived owner superseded by its successor.
	s := recordStore(t)
	ctx := context.Background()
	if err := s.InsertExecutionSlot(ctx, liveSlot("slt-1", "task-alpha")); err != nil {
		t.Fatal(err)
	}
	if released, err := s.ReleaseExecutionSlot(ctx, "slt-1", "released", "t", "task-alpha", "completed", "held"); err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	if err := s.InsertScopeBinding(ctx, liveBinding("bnd-1", "task-alpha")); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveScopeBinding(ctx, "bnd-1", "archived", "bnd-2", "t"); err != nil {
		t.Fatal(err)
	}
	// When: the same subject and scope take new live rows.
	slot := liveSlot("slt-2", "task-beta")
	slot.Tenure = 2
	// Then: the partial indexes admit them, as they only guard live rows.
	if err := s.InsertExecutionSlot(ctx, slot); err != nil {
		t.Fatalf("new tenure after release: %v", err)
	}
	if err := s.InsertScopeBinding(ctx, liveBinding("bnd-2", "task-beta")); err != nil {
		t.Fatalf("successor owner after archive: %v", err)
	}
}
