package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

// pythonGuardScript drives each guard index from Python two ways. Eight threads on separate
// connections race the writer API that owns the table (bind_scope, register_supervision,
// MergeTurn.request, Capacity.reserve, EditRegions.propose) into a second live row: every writer
// decides under BEGIN IMMEDIATE, so each either refuses with a reason, queues or replays, and none
// reaches the index. Then the one path that does reach it: a raw INSERT of a second live row, the
// write test_linkage.py:1018 and test_managed_reservation.py:405 make, both bare and inside
// Store.transaction(). It prints, per index, what escaped the race and what the raw write raised,
// with the store path and the forced row so Go can write the same row into the same store.
const pythonGuardScript = `
import json, os, sys, threading, sqlite3
from codex_session_relay.store import Store
from codex_session_relay.clock import FakeClock
from codex_session_relay.models import Endpoint
from codex_session_relay.errors import RelayError
from codex_session_relay.linkage import Linkage, PARENT
from codex_session_relay.mergeturn import MergeTurn
from codex_session_relay.capacity import Capacity
from codex_session_relay.editregion import EditRegions
root = sys.argv[1]
a, b, z, sup = (Endpoint("task-alpha", "host-a", cwd="/a"), Endpoint("task-beta", "host-b", cwd="/b"),
                Endpoint("task-zeta", "host-z", cwd="/z"), Endpoint("task-sup", "host-s", cwd="/s"))
def supervised(l):
    for key, ep in (("PRJ-A", a), ("PRJ-B", b)):
        l.register_supervision(initiative_key="INIT-1", project_key=key, supervisor=sup, parent=ep)
def peers(l):
    for key, ep in (("PRJ-A", a), ("PRJ-B", b)):
        l.bind_scope(role=PARENT, scope_key=key, endpoint=ep)
    return l.register_peer(left_project="PRJ-A", left_parent=a, right_project="PRJ-B", right_parent=b)["linkId"]
def propose(st, l, link):
    return EditRegions(st, FakeClock(), l).propose(repository="repo", base_revision="rev1", path="src/x.py", region_kind="file", region_key="", region_class="source", regenerate_from=None, left_project="PRJ-A", right_project="PRJ-B", peer_link_id=link, proposer_task_id=a.task_id, constraint_text="keep it", issue_key="CRW-1", next_owner=b.task_id)
def request(st, l, ep, proj):
    return MergeTurn(st, FakeClock(), l).request(repository="repo", base_ref="main", project_key=proj, holder=ep, candidate_head="h1", ready=True)
# index -> (setup(store, linkage) -> ctx, API racers [(fn(store, linkage, ctx))], table, where-first, overrides)
cases = {
 "scope_bindings_one_live_owner": (lambda s, l: None,
     [lambda st, l, c: l.bind_scope(role=PARENT, scope_key="PRJ-A", endpoint=a), lambda st, l, c: l.bind_scope(role=PARENT, scope_key="PRJ-A", endpoint=b)],
     "scope_bindings", {"binding_id": "bnd-forced", "task_id": "task-forced"}),
 "scope_links_one_live_edge": (lambda s, l: None,
     [lambda st, l, c: l.register_supervision(initiative_key="INIT-1", project_key="PRJ-A", supervisor=sup, parent=a), lambda st, l, c: l.register_supervision(initiative_key="INIT-1", project_key="PRJ-A", supervisor=sup, parent=b)],
     "scope_links", {"link_id": "lnk-forced", "lower_task_id": "task-forced"}),
 "merge_turns_one_live_holder": (lambda s, l: supervised(l),
     [lambda st, l, c: request(st, l, a, "PRJ-A"), lambda st, l, c: request(st, l, b, "PRJ-B")],
     "merge_turns", {"turn_id": "mtn-forced", "holder_task_id": "task-forced", "state": "holding"}),
 "merge_turns_one_live_claim": (lambda s, l: supervised(l),
     [lambda st, l, c: request(st, l, a, "PRJ-A"), lambda st, l, c: request(st, l, a, "PRJ-A")],
     "merge_turns", {"turn_id": "mtn-forced", "tenure": 2, "state": "waiting"}),
 "execution_slots_one_live_subject": (lambda s, l: supervised(l),
     [lambda st, l, c: Capacity(st, FakeClock(), l).reserve(subject_kind="assignment", subject_key="S1", parent_task_id=a.task_id, project_key="PRJ-A", reserved_by=a.task_id)] * 2,
     "execution_slots", {"slot_id": "slt-forced", "tenure": 2}),
 "edit_agreements_one_live_per_region": (lambda s, l: peers(l),
     [lambda st, l, c: propose(st, l, c)] * 2,
     "edit_agreements", {"agreement_id": "agr-forced", "tenure": 2}),
}
def surface(e):
    return {"type": type(e).__name__, "message": str(e), "code": getattr(e, "sqlite_errorcode", None)}
out = {}
for index, (setup, racers, table, overrides) in cases.items():
    path = os.path.join(root, index + ".sqlite3")
    s = Store(path); l = Linkage(s, FakeClock()); ctx = setup(s, l)
    # 1. The writer API, raced from separate connections, 8 threads: what escapes?
    escaped = []
    def run(fn):
        st = Store(path)
        try: fn(st, Linkage(st, FakeClock()), ctx)
        except RelayError as e: escaped.append("RelayError:" + e.reason.value)
        except BaseException as e: escaped.append(type(e).__name__ + ":" + str(e))
        finally: st.close()
    ts = [threading.Thread(target=run, args=(racers[i % len(racers)],)) for i in range(8)]
    [t.start() for t in ts]; [t.join(60) for t in ts]
    # 2. The raw write that reaches the index (what test_linkage.py:1018 does), bare and in a transaction.
    s.db.row_factory = sqlite3.Row
    first = dict(s.db.execute(f"SELECT * FROM {table} ORDER BY rowid LIMIT 1").fetchone())
    row = dict(first, **overrides)
    sql = f"INSERT INTO {table} ({', '.join(row)}) VALUES ({', '.join('?' * len(row))})"
    raw = tx = None
    try: s.db.execute(sql, tuple(row.values()))
    except BaseException as e: raw = surface(e)
    try:
        with s.transaction() as db:
            db.execute(sql, tuple(row.values()))
    except BaseException as e: tx = surface(e)
    live = s.db.execute(f"SELECT COUNT(*) FROM {table} WHERE rowid > 0").fetchone()[0]
    forced = s.db.execute(f"SELECT COUNT(*) FROM {table} WHERE {next(iter(overrides))} = ?", (next(iter(overrides.values())),)).fetchone()[0]
    s.close()
    out[index] = {"db": path, "row": {k: v for k, v in row.items()}, "apiRace": sorted(set(x for x in escaped if not x.startswith("RelayError"))),
                  "apiRefusals": sorted(set(x for x in escaped if x.startswith("RelayError"))),
                  "raw": raw, "transaction": tx, "forcedRowsLeft": forced}
print(json.dumps(out, sort_keys=True))
`

// pythonGuardSurface is what Python surfaced for one guard index.
type pythonGuardSurface struct {
	DB          string         `json:"db"`
	Row         map[string]any `json:"row"`
	APIRace     []string       `json:"apiRace"`
	APIRefusals []string       `json:"apiRefusals"`
	Raw         *pyError       `json:"raw"`
	Transaction *pyError       `json:"transaction"`
	ForcedLeft  int            `json:"forcedRowsLeft"`
}

type pyError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func pythonGuardSurfaces(t *testing.T) map[string]pythonGuardSurface {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("TMPDIR", root)
	out := pythonStoreValue(t, pythonGuardScript, root)
	var surfaces map[string]pythonGuardSurface
	if err := json.Unmarshal([]byte(out[strings.LastIndex(out, "\n")+1:]), &surfaces); err != nil {
		t.Fatalf("python output %q: %v", out, err)
	}
	return surfaces
}

// guardIndexes are store.py GUARD_INDEXES with the table each guards.
var guardIndexes = map[string]string{
	"scope_bindings_one_live_owner":       "scope_bindings",
	"scope_links_one_live_edge":           "scope_links",
	"merge_turns_one_live_holder":         "merge_turns",
	"merge_turns_one_live_claim":          "merge_turns",
	"execution_slots_one_live_subject":    "execution_slots",
	"edit_agreements_one_live_per_region": "edit_agreements",
}

// runGuard proves Go surfaces a write that reaches index exactly as Python does.
func runGuard(t *testing.T, index string) {
	t.Helper()
	table := guardIndexes[index]
	// Given: Python's writer API, raced from eight connections, never reached the index (every
	// racer refused with a reason, queued or replayed), and Python's raw write of the second live
	// row raised sqlite3.IntegrityError 2067, bare and inside Store.transaction(), keeping nothing.
	python := pythonGuardSurfaces(t)[index]
	if len(python.APIRace) != 0 {
		t.Fatalf("Python's writer API reached %s under a race: %v", index, python.APIRace)
	}
	for _, surface := range []*pyError{python.Raw, python.Transaction} {
		if surface == nil || surface.Type != "IntegrityError" || surface.Code != 2067 || !strings.HasPrefix(surface.Message, "UNIQUE constraint failed: "+table+".") {
			t.Fatalf("Python raw write into %s: %+v", index, surface)
		}
	}
	if python.ForcedLeft != 0 {
		t.Fatalf("Python kept %d forced rows", python.ForcedLeft)
	}
	// When: Go opens that Python-written store and writes the same row through the typed insert,
	// bare and inside Transaction.
	s, err := Open(context.Background(), python.DB, "")
	must(t, err)
	defer func() { must(t, s.Close()) }()
	ctx := context.Background()
	insert := guardInsert(t, table, python.Row)
	bare := insert(ctx, s)
	transacted := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return insert(ctx, s) })
	// Then: the same surface. The driver's constraint error with the same extended code and the
	// same constraint text, never a refusal reason, and nothing written.
	for _, err := range []error{bare, transacted} {
		var driverErr *sqlite.Error
		if !errors.As(err, &driverErr) || driverErr.Code() != python.Raw.Code || !strings.Contains(err.Error(), python.Raw.Message) || RefusalReason(err) != "" {
			t.Fatalf("Go second live row in %s: %v, want Python's IntegrityError %d %q", index, err, python.Raw.Code, python.Raw.Message)
		}
	}
	var forced int
	key := guardKeyColumn(table)
	must(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+key+" = ?", python.Row[key]).Scan(&forced))
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
