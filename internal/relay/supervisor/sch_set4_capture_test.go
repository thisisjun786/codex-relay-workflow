package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func captureAttemptSet4(t *testing.T, c *Channel, id string, h SendAdapter, at float64, token int) map[string]any {
	t.Helper()
	previous := TokenSource
	b := make([]byte, 8)
	b[7] = byte(token)
	TokenSource = bytes.NewReader(b)
	t.Cleanup(func() { TokenSource = previous })
	c.Settings = &delivery.TaskSettings{}
	answer, err := c.Attempt(context.Background(), id, h, at)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func captureHandoverSet4(t *testing.T, c *Channel, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	original := "bnd-24179d1961baacd1886d337c38c3eceb"
	successor := "bnd-b5f442e2faff9adf98768ffc7139b265"
	if err := s.ArchiveScopeBinding(ctx, original, "archived", successor, captureAt57); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertScopeBinding(ctx, store.ScopeBindingsRow{BindingID: successor, Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "01successor-supervisor", HostID: "host-a", Status: "active", Revision: 2, CreatedAt: captureAt57, UpdatedAt: captureAt57, CWD: sql.NullString{String: "/successor", Valid: true}, CXCSession: sql.NullString{String: "cxc-next", Valid: true}, HandoverNote: sql.NullString{String: "the initiative changed hands", Valid: true}, Supersedes: sql.NullString{String: original, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ kind, detail string }{{"scope_bound", `{"role": "supervisor", "scopeKind": "initiative", "scopeKey": "INI-1", "taskId": "01successor-supervisor", "revision": 2}`}, {"scope_handover", `{"scopeKey": "INI-1", "from": "01supervisor-task", "to": "01successor-supervisor", "actor": "a test", "acknowledged": []}`}} {
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureAt57, entry.kind, successor, entry.detail); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RepointScopeLink(ctx, "lnk-6fa68afd8cc27a8e7780d400a52f73c2", "active", "01parent-task", "01successor-supervisor", captureAt57); err != nil {
		t.Fatal(err)
	}
}
func Test24_SCH_56_LiveArchivedAtTransport(t *testing.T) {
	supervisorMirror(t, "WhatTheFifthIndependentReviewFound.test_an_assignment_archived_between_the_claim_and_the_transport_sends_nothing", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		_, id := captureStage57(t, c, s)
		c.beforeTransport = func() {
			if _, err := s.DB.ExecContext(ctx, "UPDATE relationships SET status='archived',updated_at=? WHERE relationship_id=?", captureAt57, captureRelationID(t, c)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, "UPDATE scope_bindings SET status='archived',updated_at=? WHERE scope_kind='issue'", captureAt57); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, "UPDATE scope_links SET status='archived',updated_at=? WHERE lower_kind='issue'", captureAt57); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'scope_lifecycle',?,?)", captureAt57, captureRelationID(t, c), `{"status": "archived", "lower": "archived", "issueKey": "REL-1"}`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'status_changed',?,?)", captureAt57, captureRelationID(t, c), `{"status": "archived", "actor": "a test"}`); err != nil {
				t.Fatal(err)
			}
		}
		previous := TokenSource
		TokenSource = bytes.NewReader(make([]byte, 8))
		t.Cleanup(func() { TokenSource = previous })
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost57{&sendHost{status: "idle"}}
		_, err := c.Attempt(ctx, id, h, 1700000000)
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("refusal %v", err)
		}
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "Nothing was sent"), []any{}, []any{attempts[0].SendAttempted, attempts[0].RetrySafe, nil}, row.State}
	})
}
func Test24_SCH_66_LiveCancelledBudget(t *testing.T) {
	supervisorMirror(t, "WhatTheSeventhIndependentReviewFound.test_a_send_the_hierarchy_cancelled_gives_its_budget_back", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		c.beforeTransport = func() { captureHandoverSet4(t, c, s) }
		previous := TokenSource
		TokenSource = bytes.NewReader(make([]byte, 8))
		t.Cleanup(func() { TokenSource = previous })
		c.Settings = &delivery.TaskSettings{}
		_, err := c.Attempt(context.Background(), id, &captureHost57{&sendHost{status: "idle"}}, 1700000000)
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("refusal %v", err)
		}
		var sends int
		var last sql.NullFloat64
		err = s.DB.QueryRow("SELECT sends,last_send_at FROM recipient_rate WHERE recipient_task_id='01supervisor-task'").Scan(&sends, &last)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		var sendAt any
		if last.Valid {
			sendAt = last.Float64
		}
		return []any{refusal.Reason, []any{sends, sendAt}}
	})
}
func Test24_SCH_62_LiveFrozenOmissionReading(t *testing.T) {
	supervisorMirror(t, "EveryLineSelectsTheStoreItWasWrittenFrom.test_an_omission_points_at_the_reading_it_froze_not_at_a_reread", "setup", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": captureRelationID(t, c), "reason": "the turn settled without a report", "selectors": map[string]any{"state": c.StoreDirectory(), "markerRoot": "/marker", "workspace": filepath.Join(filepath.Dir(c.StoreDirectory()), "work"), "assignment": "asg-1", "session": "01child-session", "turn": "turn-unreported-1"}}
		o := ObservationObligation(reading)
		if o == nil {
			t.Fatal("no obligation")
		}
		staged, err := c.StageWithReading(ctx, *o, reading, "", captureAt57)
		if err != nil {
			t.Fatal(err)
		}
		id := staged["messageId"].(string)
		shown, err := c.Show(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		from := shown["stagedFrom"].(map[string]any)
		again, err := c.StageWithReading(ctx, *o, reading, "", captureAt57)
		if err != nil {
			t.Fatal(err)
		}
		other := map[string]any{"schema": reading["schema"], "reportingState": reading["reportingState"], "relationshipId": reading["relationshipId"], "reason": reading["reason"], "selectors": map[string]any{"state": c.StoreDirectory(), "markerRoot": "/marker", "workspace": filepath.Join(filepath.Dir(c.StoreDirectory()), "work"), "assignment": "asg-1", "session": "01another-session", "turn": "turn-unreported-1"}}
		changed := ObservationObligation(other)
		_, err = c.StageWithReading(ctx, *changed, other, "", captureAt57)
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("refusal %v", err)
		}
		return []any{1, s.Path, "supervisor-show", from["reading"], strings.Contains(from["recheck"].(string), "reporting-show"), again["staged"], refusal.Reason, strings.Contains(refusal.Detail, "keeps the reading it froze")}
	})
}

func Test24_SCH_65_LiveRecoveredBeforeTransport(t *testing.T) {
	supervisorMirror(t, "WhatTheSeventhIndependentReviewFound.test_a_claim_recovered_before_its_owner_starts_the_transport_sends_nothing", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		h := &captureHost57{&sendHost{status: "idle"}}
		c.beforeTransport = func() {
			row, err := c.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			c.clockISO = func() string { return delivery.ISOOf(1700000301) }
			recovered, err := c.recoverStranded(context.Background(), row, 1700000301)
			if err != nil || recovered.State != "queued" {
				t.Fatalf("recovery %+v %v", recovered, err)
			}
		}
		first := captureAttemptSet4(t, c, id, h, 1700000000, 0)
		c.beforeTransport = nil
		row, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		second := captureAttemptSet4(t, c, id, h, 1700000301, 1)
		if len(h.sends) != 1 {
			t.Fatalf("sends: %d", len(h.sends))
		}
		return []any{first, []any{}, row.State, second["deliveryState"], len(h.sends)}
	})
}
func Test24_SCH_67_LivePacedAtTransport(t *testing.T) {
	supervisorMirror(t, "WhatTheEleventhIndependentReviewFound.test_a_send_the_budget_refuses_at_its_transport_start_is_deferred", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		h := &captureHost57{&sendHost{status: "idle"}}
		c.beforeTransport = func() {
			service := delivery.NewService(s, delivery.SystemClock{})
			if refused, err := service.ReserveSend(context.Background(), "01supervisor-task", 1700000000); err != nil || refused != "" {
				t.Fatalf("reserve %q %v", refused, err)
			}
		}
		first := captureAttemptSet4(t, c, id, h, 1700000000, 0)
		row, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := s.SupervisorAttempts(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		c.beforeTransport = nil
		c.clockISO = func() string { return captureAt57 }
		second := captureAttemptSet4(t, c, id, h, row.NextEligibleAt.Float64, 1)
		if len(h.sends) != 1 {
			t.Fatalf("sends: %d", len(h.sends))
		}
		return []any{first, nil, []any{row.State, nil}, row.NextEligibleAt.Float64, []any{}, attempts[0].SendAttempted, second["deliveryState"]}
	})
}

type capturePolicyHostSet4 struct {
	*captureHost57
	refused bool
}

func (h *capturePolicyHostSet4) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	if h.refused {
		h.refused = false
		return delivery.Obj{{Key: "status", Value: "failed"}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "untrusted"}}}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "unsupported_approval_policy"}}}}, nil
	}
	return h.captureHost57.SendMessage(id, thread, message, settings)
}
func Test24_SCH_74_LivePolicyRefusal(t *testing.T) {
	supervisorMirror(t, "WhatTheFourteenthIndependentReviewFound.test_a_push_the_recipients_policy_refuses_is_not_sent_and_goes_later", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		h := &capturePolicyHostSet4{captureHost57: &captureHost57{&sendHost{status: "idle"}}, refused: true}
		answer := captureAttemptSet4(t, c, id, h, 1700000000, 0)
		reach, err := c.Reach(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		c.clockISO = func() string { return captureAt57 }
		again := captureAttemptSet4(t, c, id, h, row.NextEligibleAt.Float64, 1)
		reached, err := c.Reach(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{false, []any{answer["deliveryState"], answer["sendAttempted"], answer["turnId"]}, answer["transportDeliveryState"], reach["transport_accepted"].(map[string]any)["state"], []any{row.State, nil}, again["deliveryState"], reached["transport_accepted"].(map[string]any)["state"]}
	})
}
func Test24_SCH_72_LiveSettingsAtTransport(t *testing.T) {
	supervisorMirror(t, "WhatTheThirteenthIndependentReviewFound.test_settings_changed_between_the_claim_and_the_transport_start_are_not_sent", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		_, id := captureStage57(t, c, s)
		var old string
		if err := s.DB.QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id='01supervisor-task'").Scan(&old); err != nil {
			t.Fatal(err)
		}
		current := &delivery.TaskSettings{Data: delivery.Obj{{Key: "cwd", Value: "/supervisor"}}}
		c.SettingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) { return current, nil }
		c.beforeTransport = func() {
			current = &delivery.TaskSettings{Data: delivery.Obj{{Key: "cwd", Value: "/changed-after-gate"}}}
			replacement := strings.ReplaceAll(old, "/supervisor", "/changed-after-gate")
			if _, err := s.DB.ExecContext(ctx, "UPDATE authorized_settings SET settings=?,source='user_transition' WHERE task_id='01supervisor-task'", replacement); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'settings_recorded','01supervisor-task',?)", captureAt57, `{"source": "user_transition"}`); err != nil {
				t.Fatal(err)
			}
		}
		previous := TokenSource
		TokenSource = bytes.NewReader(make([]byte, 8))
		t.Cleanup(func() { TokenSource = previous })
		h := &captureHost57{&sendHost{status: "idle"}}
		first, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		c.beforeTransport = nil
		TokenSource = bytes.NewReader([]byte{0, 0, 0, 0, 0, 0, 0, 1})
		second, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{first, []any{}, []any{attempts[0].SendAttempted, attempts[0].RetrySafe, nil}, []any{row.State, nil}, second["deliveryState"], h.settings.Data[0].Value, len(h.sends)}
	})
}
func Test24_SCH_69_LiveTokenPerAttempt(t *testing.T) {
	supervisorMirror(t, "WhatTheNinthIndependentReviewFound.test_the_token_is_drawn_per_attempt_and_carried_in_its_bytes", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		h := &captureHost57{&sendHost{status: "idle"}}
		captureAttemptSet4(t, c, id, &presendRecoveryHost24{sendHost: h.sendHost}, 1700000000, 0)
		c.clockISO = func() string { return delivery.ISOOf(1700086400) }
		captureAttemptSet4(t, c, id, h, 1700086400, 1)
		attempts, err := s.SupervisorAttempts(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(attempts[1].Message, "deliveryToken: "+attempts[1].DeliveryToken.String) {
			t.Fatal("second token missing from sent bytes")
		}
		return []any{attempts[0].DeliveryToken.String, attempts[0].DeliveryToken.String != attempts[1].DeliveryToken.String}
	})
}
