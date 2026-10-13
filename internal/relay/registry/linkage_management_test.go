package registry

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The management seat (CRW-1333, design CRW-1314 §1): every management session holds a seat of its
// own, role supervisor, scope kind management, keyed by the seat id (the thread that first
// registered it). The seats drive the built linkage-bind and linkage-handover lines, so the
// argparse spec, the handler and the registry are exercised together.

const managementKind = "management"

// bindManagement is one linkage-bind of task to the management seat key.
func (s *storeSeat) bindManagement(key, task string) (int, string) {
	s.t.Helper()
	return s.run("linkage-bind", "--role", "supervisor", "--scope-kind", managementKind, "--scope", key, "--task", task, "--host", "host")
}

// mustBindManagement is bindManagement with a successful answer decoded.
func (s *storeSeat) mustBindManagement(key, task string) map[string]any {
	s.t.Helper()
	code, out := s.bindManagement(key, task)
	if code != 0 {
		s.t.Fatalf("bind management %s for %s: exit %d stdout %q", key, task, code, out)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		s.t.Fatalf("binding answer is not JSON: %v (%q)", err, out)
	}
	return answer
}

// rows reads a query over this test's store as one map per row.
func (s *storeSeat) rows(query string, args ...any) []store.Row {
	s.t.Helper()
	held, err := store.Open(context.Background(), filepath.Join(s.state, "relay.sqlite3"), "")
	if err != nil {
		s.t.Fatal(err)
	}
	defer held.Close()
	rows, err := held.All(context.Background(), query, args...)
	if err != nil {
		s.t.Fatal(err)
	}
	return rows
}

// TestManagementSeatPerThread: two management sessions each hold their own seat, and neither
// overwrites the other. Before CRW-1333 --scope-kind management was a parser refusal.
func TestManagementSeatPerThread(t *testing.T) {
	s := newStoreSeat(t)
	first := s.mustBindManagement("01mgmtA", "01mgmtA")
	second := s.mustBindManagement("01mgmtB", "01mgmtB")
	if first["scopeKind"] != managementKind || first["scopeKey"] != "01mgmtA" || first["taskId"] != "01mgmtA" {
		t.Fatalf("first seat %v", first)
	}
	if second["scopeKind"] != managementKind || second["scopeKey"] != "01mgmtB" || second["taskId"] != "01mgmtB" {
		t.Fatalf("second seat %v", second)
	}
	live := s.rows("SELECT task_id FROM scope_bindings WHERE scope_kind = 'management' AND status = 'active' AND superseded_by IS NULL ORDER BY task_id")
	if len(live) != 2 || live[0].Text("task_id") != "01mgmtA" || live[1].Text("task_id") != "01mgmtB" {
		t.Fatalf("live management seats %v, want both sessions", live)
	}
	// A third session cannot take a seat another one holds: replacing an owner is a handover.
	s.mustRefuse("duplicate_scope_owner", "linkage-bind", "--role", "supervisor", "--scope-kind", managementKind, "--scope", "01mgmtA", "--task", "01other", "--host", "host")
	// The store seat keeps its own rule: only the store key names it.
	s.mustRefuse("scope_role_mismatch", "linkage-bind", "--role", "parent", "--scope-kind", managementKind, "--scope", "01mgmtC", "--task", "01mgmtC", "--host", "host")
}

// TestManagementSeatHandoverIsRecorded: a new session takes an old seat by linkage-handover, and the
// move is a record: the old binding archived and superseded by the new one, the new one carrying the
// handover note, and a scope_handover journal row.
func TestManagementSeatHandoverIsRecorded(t *testing.T) {
	s := newStoreSeat(t)
	old := s.mustBindManagement("01mgmtA", "01mgmtA")
	answer := s.mustHandover("supervisor", "01mgmtA", managementKind, "01mgmtA", "01mgmtNew")
	if answer["scopeKind"] != managementKind || answer["scopeKey"] != "01mgmtA" || answer["taskId"] != "01mgmtNew" {
		t.Fatalf("handover answered %v", answer)
	}
	if answer["handoverNote"] != "the seat changes hands" || answer["supersedes"] != old["bindingId"] {
		t.Fatalf("the new binding carries note %v supersedes %v, want the evidence and %v", answer["handoverNote"], answer["supersedes"], old["bindingId"])
	}
	archived := s.rows("SELECT status, superseded_by FROM scope_bindings WHERE binding_id = ?", old["bindingId"])
	if len(archived) != 1 || archived[0].Text("status") != "archived" || archived[0].Text("superseded_by") != answer["bindingId"] {
		t.Fatalf("old binding %v, want archived and superseded by %v", archived, answer["bindingId"])
	}
	if journal := s.rows("SELECT subject FROM journal WHERE kind = 'scope_handover'"); len(journal) != 1 || journal[0].Text("subject") != answer["bindingId"] {
		t.Fatalf("scope_handover journal %v", journal)
	}
	// The successor registering its own seat again reads back the seat it inherited rather than
	// opening a second one under its own id.
	again := s.mustBindManagement("01mgmtNew", "01mgmtNew")
	if again["bindingId"] != answer["bindingId"] || again["scopeKey"] != "01mgmtA" {
		t.Fatalf("re-registering the successor gave %v, want the inherited seat %v", again, answer["bindingId"])
	}
}

// TestManagementSeatRetiresTheStoreSeatOfTheSameThread: one task holds one supervisor seat, so a
// thread holding the store seat that registers its management seat lays the store seat down, with
// the record of where it went.
func TestManagementSeatRetiresTheStoreSeatOfTheSameThread(t *testing.T) {
	s := newStoreSeat(t)
	storeBinding := s.mustBind("01mgmtA", storeSeatKind)
	seat := s.mustBindManagement("01mgmtA", "01mgmtA")
	retired := s.rows("SELECT status, superseded_by FROM scope_bindings WHERE binding_id = ?", storeBinding["bindingId"])
	if len(retired) != 1 || retired[0].Text("status") != "archived" || retired[0].Text("superseded_by") != seat["bindingId"] {
		t.Fatalf("store seat %v, want archived and superseded by the management seat %v", retired, seat["bindingId"])
	}
	moved := s.rows("SELECT subject, detail FROM journal WHERE kind = 'scope_seat_moved'")
	if len(moved) != 1 || moved[0].Text("subject") != seat["bindingId"] {
		t.Fatalf("scope_seat_moved journal %v", moved)
	}
	if owner := s.seatOwner("01mgmtA"); owner != "01mgmtA" {
		t.Fatalf("the thread holds no live supervisor seat after the move (%q)", owner)
	}
	// Another thread's store seat is not touched: the move is the registering thread's own.
	s.mustBind("01store", storeSeatKind)
	s.mustBindManagement("01mgmtB", "01mgmtB")
	if live := s.rows("SELECT task_id FROM scope_bindings WHERE scope_kind = 'store' AND status = 'active'"); len(live) != 1 || live[0].Text("task_id") != "01store" {
		t.Fatalf("live store seats %v, want 01store's untouched", live)
	}
}
