package store

import (
	"context"
	"database/sql"
	"testing"
)

// sendingRow inserts a message sending on attempt 1 under owner-a, with no hold.
func sendingRow(t *testing.T, s *Store, id string) {
	t.Helper()
	_, err := s.DB.ExecContext(context.Background(), "INSERT INTO supervisor_messages (message_id, obligation_id,"+
		" obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet,"+
		" state, attempt_count, lease_owner, lease_until, staged_at, updated_at)"+
		" VALUES (?,?,'report','rel','p','k','parent','sup','s','{}','sending',1,'owner-a',50,'t1','t1')", id, "obl-"+id)
	must(t, err)
}

func TestSettleSupervisorMessageKeepsAHoldSetWhileSending(t *testing.T) {
	t.Parallel()
	// Given: one sending row given a hold by SQL while it was sending, and one sending row with no hold.
	s := recordStore(t)
	ctx := context.Background()
	sendingRow(t, s, "held")
	sendingRow(t, s, "free")
	_, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET hold_reason = ? WHERE message_id = 'held'", SupervisorHoldSuperseded)
	must(t, err)
	// When: the held row settles with a NULL hold and then a cap word is offered to the free row.
	movedHeld, err := s.SettleSupervisorMessage(ctx, "held", "dispatched", sql.NullFloat64{}, sql.NullString{}, "t2", "sending", 1, text("owner-a"))
	must(t, err)
	movedFree, err := s.SettleSupervisorMessage(ctx, "free", "withheld_pre_send", sql.NullFloat64{Float64: 70, Valid: true}, text("cap"), "t2", "sending", 1, text("owner-a"))
	must(t, err)
	held, err := s.SupervisorMessage(ctx, "held")
	must(t, err)
	free, err := s.SupervisorMessage(ctx, "free")
	must(t, err)
	// Then: both rows move and release the lease; the held word stays, and the cap is written on the NULL hold.
	if !movedHeld || held.State != "dispatched" || held.HoldReason != text(SupervisorHoldSuperseded) || held.LeaseOwner.Valid || held.LeaseUntil.Valid {
		t.Fatalf("held: moved=%v row=%+v", movedHeld, held)
	}
	if !movedFree || free.State != "withheld_pre_send" || free.HoldReason != text("cap") || free.NextEligibleAt.Float64 != 70 || free.LeaseOwner.Valid {
		t.Fatalf("free: moved=%v row=%+v", movedFree, free)
	}
}

func TestSettleSupervisorMessageKeepsAHoldOverADifferentWord(t *testing.T) {
	t.Parallel()
	// Given: a sending row given a hold by SQL while it was sending.
	s := recordStore(t)
	ctx := context.Background()
	sendingRow(t, s, "held")
	_, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET hold_reason = ? WHERE message_id = 'held'", SupervisorHoldSuperseded)
	must(t, err)
	// When: it settles with a different hold word.
	moved, err := s.SettleSupervisorMessage(ctx, "held", "withheld_pre_send", sql.NullFloat64{Float64: 70, Valid: true}, text("cap"), "t2", "sending", 1, text("owner-a"))
	must(t, err)
	row, err := s.SupervisorMessage(ctx, "held")
	must(t, err)
	// Then: the state moves, and the hold that was present is kept.
	if !moved || row.State != "withheld_pre_send" || row.HoldReason != text(SupervisorHoldSuperseded) || row.NextEligibleAt.Float64 != 70 {
		t.Fatalf("moved=%v row=%+v", moved, row)
	}
}
