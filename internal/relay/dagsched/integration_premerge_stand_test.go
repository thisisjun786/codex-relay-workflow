package dagsched

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// CRW-952 c4 (d3): after a base refresh whose merge is hand-resolved, integration judges the stand head, not the head the
// record was stored for. The candidate's record is held with premerge_head_mismatch.

func TestPremergeStandHeadAfterAHandResolvedRefreshIsJudgedAndHeld(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	// the scenario's acceptance was made without a record: store the record of the accepted head, in the stored shape
	raw := premergeAt(s.sched, context.Background(), "g", "I", s.h1)
	digest, err := premerge.Digest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.s.DB.Exec("INSERT INTO dag_acceptance_premerge (acceptance_id, record_digest, record_json, evaluated_head, accepted_head, recorded_by, coordinator_epoch, recorded_at) VALUES (?,?,?,?,?,?,?,?)",
		s.accepted.Acceptance.AcceptanceID, digest, string(raw), s.h1, s.h1, "parent", 0, "t"); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatalf("the refresh record: %v", err)
	}
	held, err := s.sched.premergeLeftOut(context.Background(), "g")
	if err != nil {
		t.Fatalf("premergeLeftOut: %v", err)
	}
	if len(held) != 1 || held[0].NodeID != "I" || held[0].Reason != "premerge_head_mismatch" {
		t.Fatalf("the refreshed stand head must hold the record of the old head: %+v", held)
	}
}
