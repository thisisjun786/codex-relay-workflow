package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// CRW-1486/S4 (finding 9): the store half of supervisor-list. The list reads one recipient's messages
// through the unsent test the channel already writes.

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

// TestListSupervisorMessagesProjectNoneReadsNull (CRW-1500): --project none lists the set the
// supervisor-list count calls none, the messages with no project and the messages of a project
// literally named none, and no other project's.
func TestListSupervisorMessagesProjectNoneReadsNull(t *testing.T) {
	s := recordStore(t)
	seedListRows(t, s, "R",
		listSeed{"n1", "completion", "notification", "", "queued", "", "2026-10-13T12:50:01.000000+00:00"},
		listSeed{"p1", "completion", "notification", "P1", "queued", "", "2026-10-13T12:50:02.000000+00:00"},
		listSeed{"n2", "completion", "notification", SupervisorListProjectNone, "queued", "", "2026-10-13T12:50:03.000000+00:00"},
	)
	seedListRows(t, s, "OTHER", listSeed{"x1", "completion", "notification", "", "queued", "", "2026-10-13T12:50:00.000000+00:00"})
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		filter SupervisorListFilter
		want   []string
	}{
		{"none", SupervisorListFilter{Recipient: "R", Project: SupervisorListProjectNone}, []string{"n1", "n2"}},
		{"none, all states", SupervisorListFilter{Recipient: "R", Project: SupervisorListProjectNone, State: SupervisorListAll}, []string{"n1", "n2"}},
		{"none, no hold", SupervisorListFilter{Recipient: "R", Project: SupervisorListProjectNone, Hold: SupervisorListHoldNone}, []string{"n1", "n2"}},
		{"P1", SupervisorListFilter{Recipient: "R", Project: "P1"}, []string{"p1"}},
		{"every project", SupervisorListFilter{Recipient: "R"}, []string{"n1", "p1", "n2"}},
	} {
		rows, err := s.ListSupervisorMessages(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := listedIDs(rows); !slices.Equal(got, tc.want) {
			t.Errorf("%s listed %v, want %v", tc.name, got, tc.want)
		}
	}
}
