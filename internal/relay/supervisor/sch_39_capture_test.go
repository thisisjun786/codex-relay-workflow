package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test24_SCH_39_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "WhatTheFifthReviewRoundFound.test_a_report_already_attempted_for_the_former_supervisor_stays_frozen", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		c.Settings = &delivery.TaskSettings{}
		one := captureObligation4(t, c, s)
		staged, err := c.Stage(ctx, one, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := staged["messageId"].(string)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		if _, err = c.Attempt(ctx, id, h, 1700000000); err != nil {
			t.Fatal(err)
		}
		original := "bnd-24179d1961baacd1886d337c38c3eceb"
		successor := "bnd-b5f442e2faff9adf98768ffc7139b265"
		if err = storeseed.ArchiveScopeBinding(ctx, s, original, "archived", successor, captureTime); err != nil {
			t.Fatal(err)
		}
		if err = storeseed.InsertScopeBinding(ctx, s, store.ScopeBindingsRow{BindingID: successor, Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "01successor-supervisor", HostID: "host-a", Status: "active", Revision: 2, CreatedAt: captureTime, UpdatedAt: captureTime, CWD: sql.NullString{String: "/successor", Valid: true}, CXCSession: sql.NullString{String: "cxc-next", Valid: true}, HandoverNote: sql.NullString{String: "the initiative changed hands", Valid: true}, Supersedes: sql.NullString{String: original, Valid: true}}); err != nil {
			t.Fatal(err)
		}
		for _, entry := range []struct{ kind, detail string }{
			{"scope_bound", `{"role": "supervisor", "scopeKind": "initiative", "scopeKey": "INI-1", "taskId": "01successor-supervisor", "revision": 2}`},
			{"scope_handover", `{"scopeKey": "INI-1", "from": "01supervisor-task", "to": "01successor-supervisor", "actor": "a test", "acknowledged": []}`},
		} {
			if _, err = s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureTime, entry.kind, successor, entry.detail); err != nil {
				t.Fatal(err)
			}
		}
		if err = storeseed.RepointScopeLink(ctx, s, "lnk-6fa68afd8cc27a8e7780d400a52f73c2", "active", "01parent-task", "01successor-supervisor", captureTime); err != nil {
			t.Fatal(err)
		}
		_, err = c.Stage(ctx, one, "", captureTime)
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("expected refusal: %v", err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM journal WHERE kind='supervisor_message_readdressed'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		standing, err := c.StageStanding(ctx, "PRJ-1", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		reasons := []any{}
		for _, item := range standing["refused"].([]any) {
			reasons = append(reasons, item.(map[string]any)["reason"])
		}
		if n != 0 {
			t.Fatalf("readdressed journal count: %d", n)
		}
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "never re-addressed"), row.RecipientTaskID, len(attempts), []any{}, reasons}
	})
}
