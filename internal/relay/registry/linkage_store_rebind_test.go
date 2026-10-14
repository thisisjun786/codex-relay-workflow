package registry

import "testing"

// TestStoreSeatRebindAfterTheManagementSeatsHandoverIsLive (CRW-1361): a thread lays its store seat
// down when it registers its management seat (archived, superseded_by the management binding), hands
// the management seat over, and binds the store seat again. The rebind reactivates the archived row,
// so it has to clear superseded_by too: a row that is active but still superseded is no live seat for
// any reader, and StoreScopeSupervisor answered nobody at all although the bind answered active.
func TestStoreSeatRebindAfterTheManagementSeatsHandoverIsLive(t *testing.T) {
	s := newStoreSeat(t)
	t.Setenv("CODEX_THREAD_ID", "")
	first := s.mustBind("01mgmtA", storeSeatKind)
	seat := s.mustBindManagement("01mgmtA", "01mgmtA")
	s.mustHandover("supervisor", "01mgmtA", managementKind, "01mgmtA", "01mgmtNew")
	if got := s.storeSeatHolders(); len(got) != 1 || got[0] != "01mgmtNew@management" {
		t.Fatalf("store seat after the handover %v, want the management seat's new owner", got)
	}
	again := s.mustBind("01mgmtA", storeSeatKind)
	if again["bindingId"] != first["bindingId"] || again["status"] != "active" || again["supersededBy"] != nil {
		t.Fatalf("the rebind answered %v, want the same binding %v active and superseded by nothing (was superseded by %v)", again, first["bindingId"], seat["bindingId"])
	}
	rows := s.rows("SELECT status, superseded_by FROM scope_bindings WHERE binding_id = ?", first["bindingId"])
	if len(rows) != 1 || rows[0].Text("status") != "active" || rows[0].Get("superseded_by") != nil {
		t.Fatalf("store seat row %v, want active with superseded_by NULL", rows)
	}
	if got := s.storeSeatHolders(); len(got) != 1 || got[0] != "01mgmtA@store" {
		t.Fatalf("store seat after the rebind %v, want the rebound store seat's holder", got)
	}
	if rebound := s.rows("SELECT subject FROM journal WHERE kind = 'scope_rebound' AND subject = ?", first["bindingId"]); len(rebound) != 1 {
		t.Fatalf("scope_rebound journal %v, want one row for the rebind", rebound)
	}
}
