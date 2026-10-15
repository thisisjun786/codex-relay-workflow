package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// CRW-1486/S4 (findings 6 and 9): the store half of supervisor-list and supervisor-acknowledge. The
// list reads one recipient's messages through the unsent test the channel already writes, and the
// acknowledgement parks a row under the existing superseded hold with the existing journal kind, in
// one transaction, only while nothing of the row can have gone.

type listSeed struct {
	id, obligationKind, kind, project, state, hold, stagedAt string
}

func seedListRows(t *testing.T, s *Store, recipient string, seeds ...listSeed) {
	t.Helper()
	ctx := context.Background()
	for _, seed := range seeds {
		project := sql.NullString{String: seed.project, Valid: seed.project != ""}
		if _, err := s.StageSupervisorMessage(ctx, SupervisorMessagesRow{MessageID: seed.id, ObligationID: "obl-" + seed.id, ObligationKind: seed.obligationKind,
			RelationshipID: "rel-" + seed.project, ProjectKey: project, Purpose: "p", Kind: seed.kind, SenderTaskID: "sender", RecipientTaskID: recipient,
			Subject: "subject-" + seed.id, Packet: "{}", State: "queued", StagedAt: seed.stagedAt, UpdatedAt: seed.stagedAt}); err != nil {
			t.Fatal(err)
		}
		var hold any
		if seed.hold != "" {
			hold = seed.hold
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state=?,hold_reason=? WHERE message_id=?", seed.state, hold, seed.id); err != nil {
			t.Fatal(err)
		}
	}
}

func listedIDs(rows []SupervisorMessagesRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.MessageID)
	}
	return ids
}

// TestListSupervisorMessagesReadsOneRecipientsQueue: the unsent rows of one recipient in staging
// order by default, every row with all, and the project and hold filters narrow it.
func TestListSupervisorMessagesReadsOneRecipientsQueue(t *testing.T) {
	s := recordStore(t)
	seedListRows(t, s, "R",
		listSeed{"m3", "completion", "notification", "P1", "queued", "", "2026-10-13T12:50:03.000000+00:00"},
		listSeed{"m1", "completion", "notification", "P1", "deferred_busy", "", "2026-10-13T12:50:01.000000+00:00"},
		listSeed{"m2", "blocked", "notification", "P2", "queued", SupervisorHoldSuperseded, "2026-10-13T12:50:02.000000+00:00"},
		listSeed{"m4", "completion", "notification", "P1", "dispatched", "", "2026-10-13T12:50:04.000000+00:00"},
		listSeed{"m5", "completion", "notification", "P2", "sending", "", "2026-10-13T12:50:05.000000+00:00"},
	)
	seedListRows(t, s, "OTHER", listSeed{"x1", "completion", "notification", "P1", "queued", "", "2026-10-13T12:50:00.000000+00:00"})
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		filter SupervisorListFilter
		want   []string
	}{
		{"unsent", SupervisorListFilter{Recipient: "R"}, []string{"m1", "m2", "m3"}},
		{"all", SupervisorListFilter{Recipient: "R", State: SupervisorListAll}, []string{"m1", "m2", "m3", "m4", "m5"}},
		{"pending", SupervisorListFilter{Recipient: "R", State: SupervisorListPending}, []string{"m1", "m2", "m3", "m5"}},
		{"project", SupervisorListFilter{Recipient: "R", Project: "P1"}, []string{"m1", "m3"}},
		{"no hold", SupervisorListFilter{Recipient: "R", Hold: SupervisorListHoldNone}, []string{"m1", "m3"}},
		{"one hold", SupervisorListFilter{Recipient: "R", Hold: SupervisorHoldSuperseded}, []string{"m2"}},
	} {
		rows, err := s.ListSupervisorMessages(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := listedIDs(rows); !slices.Equal(got, tc.want) {
			t.Errorf("%s listed %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := s.ListSupervisorMessages(ctx, SupervisorListFilter{Recipient: "R", State: "settled"}); err == nil {
		t.Error("a state the list does not know was accepted")
	}
}

// TestAcknowledgeSupervisorMessagesParksOnlyANeverSentRow: the guarded update parks an unsent,
// unheld row nothing of which can have gone, writes one supervisor_message_superseded row with the
// detail it was given, and leaves a held row, a sending row, a row with an attempt that may have
// gone, a decision and a fault notice exactly as they were.
func TestAcknowledgeSupervisorMessagesParksOnlyANeverSentRow(t *testing.T) {
	s := recordStore(t)
	seedListRows(t, s, "R",
		listSeed{"park", "completion", "notification", "P1", "queued", "", "2026-10-13T12:50:01.000000+00:00"},
		listSeed{"busy", "completion", "notification", "P1", "deferred_busy", "", "2026-10-13T12:50:02.000000+00:00"},
		listSeed{"held", "completion", "notification", "P1", "queued", SupervisorHoldUnaddressed, "2026-10-13T12:50:03.000000+00:00"},
		listSeed{"sending", "completion", "notification", "P1", "sending", "", "2026-10-13T12:50:04.000000+00:00"},
		listSeed{"gone", "completion", "notification", "P1", "withheld_pre_send", "", "2026-10-13T12:50:05.000000+00:00"},
		listSeed{"decision", "decision_request", "decision", "P1", "queued", "", "2026-10-13T12:50:06.000000+00:00"},
		listSeed{"notice", SupervisorNoticeObligationKind, "notification", "P1", "queued", "", "2026-10-13T12:50:07.000000+00:00"},
	)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_attempts(request_id,message_id,attempt_no,message,state,send_attempted,retry_safe,record,sent_at,observed_at) VALUES('rq-gone','gone',1,'m','held_uncertain','unknown',0,'{}','t','t')"); err != nil {
		t.Fatal(err)
	}
	before := map[string]SupervisorMessagesRow{}
	ids := []string{"park", "busy", "held", "sending", "gone", "decision", "notice", "absent"}
	for _, id := range ids[:7] {
		row, err := s.SupervisorMessage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = row
	}
	const detail = `{"detail": "acknowledged", "by": "supervisor-acknowledge", "actor": "R"}`
	parked, err := s.AcknowledgeSupervisorMessages(ctx, ids, "2026-10-15T00:00:00.000000+00:00", detail)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"park", "busy"}; !slices.Equal(parked, want) {
		t.Fatalf("parked %v, want %v", parked, want)
	}
	for _, id := range ids[:7] {
		row, err := s.SupervisorMessage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := s.Journal(ctx, "supervisor_message_superseded", id)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(parked, id) {
			if row.HoldReason.String != SupervisorHoldSuperseded || row.State != before[id].State || row.UpdatedAt != "2026-10-15T00:00:00.000000+00:00" {
				t.Errorf("%s after the acknowledgement: %+v", id, row)
			}
			if len(entries) != 1 || entries[0].Detail != detail {
				t.Errorf("%s journaled %+v, want one row with the given detail", id, entries)
			}
			continue
		}
		if row != before[id] || len(entries) != 0 {
			t.Errorf("%s moved: %+v (was %+v), journal %+v", id, row, before[id], entries)
		}
	}
	// Again: nothing is unheld any more, so nothing moves and nothing is journaled twice.
	again, err := s.AcknowledgeSupervisorMessages(ctx, ids, "2026-10-15T00:01:00.000000+00:00", detail)
	if err != nil || len(again) != 0 {
		t.Fatalf("a second acknowledgement parked %v (%v)", again, err)
	}
}
