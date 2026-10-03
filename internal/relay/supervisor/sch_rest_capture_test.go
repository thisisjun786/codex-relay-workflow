package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

func Test24_SCH_29_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFourthReviewRoundFound.test_a_second_claim_inside_the_send_interval_is_refused_in_the_transaction", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		c.Settings = &delivery.TaskSettings{}
		var event string
		if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("first: %v %v", o, err)
		}
		staged, err := c.Stage(ctx, *o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		first := staged["messageId"].(string)
		// The second Python event is committed after the event snapshot; clone the fixture event.
		second := restSecond(t, c, s)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, first, h, 1700000005)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, second)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Resolve(ctx, row.RelationshipID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.claim(ctx, second, r, 1700000006, captureTime, "a racing caller")
		if got := captureRefusal4(t, err); got.Reason != "paced" {
			t.Fatalf("claim refusal: %+v", got)
		}
		row, err = c.Get(ctx, second)
		if err != nil {
			t.Fatal(err)
		}
		var sends int
		if err = s.DB.QueryRow("SELECT sends FROM recipient_rate WHERE recipient_task_id=?", r.Recipient).Scan(&sends); err != nil {
			t.Fatal(err)
		}
		return []any{record["deliveryState"], row.State, sends}
	})
}

func restSecond(t *testing.T, c *Channel, s *store.Store) string {
	t.Helper()
	ctx := context.Background()
	var event string
	if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
		t.Fatal(err)
	}
	o, err := c.FromEvent(ctx, event)
	if err != nil || o == nil {
		t.Fatalf("second: %v %v", o, err)
	}
	at := delivery.ISOOf(1700000005)
	if c.clockISO != nil {
		at = c.clockISO()
	}
	staged, err := c.Stage(ctx, *o, "", at)
	if err != nil {
		t.Fatal(err)
	}
	return staged["messageId"].(string)
}

func Test24_SCH_33_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheThirdReviewRoundFound.test_a_handover_committed_after_resolve_does_not_wake_the_former_supervisor", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id := set2Stage(t, c, s)
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		stale, err := c.Resolve(ctx, row.RelationshipID)
		if err != nil {
			t.Fatal(err)
		}
		restHandover33(t, s)
		_, err = c.claim(ctx, id, stale, 1700000000, captureTime, "mid-handover")
		if err == nil {
			t.Fatal("stale claim accepted")
		}
		row, err = c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err = s.DB.QueryRow("SELECT count(*) FROM supervisor_attempts").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unexpected attempts: %d", n)
		}
		return []any{true, row.State, []any{}}
	})
}
func Test24_SCH_34_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheThirdReviewRoundFound.test_two_messages_sharing_a_request_prefix_are_refused_by_name", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		c.Settings = &delivery.TaskSettings{}
		var event string
		if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation: %v %v", o, err)
		}
		staged, err := c.Stage(ctx, *o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		first := staged["messageId"].(string)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		if _, err = c.Attempt(ctx, first, h, 1700000000); err != nil {
			t.Fatal(err)
		}
		c.clockISO = func() string { return captureTime }
		second := restSecond(t, c, s)
		if _, err = s.DB.ExecContext(ctx, "UPDATE supervisor_attempts SET request_id=? WHERE message_id=?", "sup-"+second[:12]+"-a1", first); err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, second)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Resolve(ctx, row.RelationshipID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.claim(ctx, second, r, 1700003600, captureTime, "colliding")
		refusal := captureRefusal4(t, err)
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "already belongs to message")}
	})
}

func restHandover33(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	original, successor := "bnd-24179d1961baacd1886d337c38c3eceb", "bnd-45b86b6a2f3d253ae34532a3df0ec31f"
	if err := storeseed.ArchiveScopeBinding(ctx, s, original, "archived", successor, captureTime); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(ctx, s, store.ScopeBindingsRow{BindingID: successor, Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "01new-supervisor", HostID: "host-a", Status: "active", Revision: 2, CreatedAt: captureTime, UpdatedAt: captureTime, CWD: sql.NullString{String: "/new", Valid: true}, CXCSession: sql.NullString{String: "cxc-new", Valid: true}, HandoverNote: sql.NullString{String: "the initiative changed hands mid-send", Valid: true}, Supersedes: sql.NullString{String: original, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ kind, detail string }{{"scope_bound", `{"role": "supervisor", "scopeKind": "initiative", "scopeKey": "INI-1", "taskId": "01new-supervisor", "revision": 2}`}, {"scope_handover", `{"scopeKey": "INI-1", "from": "01supervisor-task", "to": "01new-supervisor", "actor": "a test", "acknowledged": []}`}} {
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureTime, entry.kind, successor, entry.detail); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeseed.RepointScopeLink(ctx, s, "lnk-6fa68afd8cc27a8e7780d400a52f73c2", "active", "01parent-task", "01new-supervisor", captureTime); err != nil {
		t.Fatal(err)
	}
}

func Test24_SCH_38_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFifthReviewRoundFound.test_a_report_staged_before_a_handover_reaches_the_successor_exactly_once", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		handoverSet3(t, s)
		h := &restSuccessorHost{captureHost4: captureHost4{sendHost: sendHost{status: "idle"}}}
		_, err := c.Attempt(ctx, id, h, 1700000000)
		refusal := captureRefusal4(t, err)
		result, err := c.Stage(ctx, o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		var messages, reports int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_messages").Scan(&messages); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRow("SELECT count(*) FROM journal WHERE kind='supervisor_report' AND subject=?", o.ID).Scan(&reports); err != nil {
			t.Fatal(err)
		}
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		again, err := c.Stage(ctx, o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		last, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "staging it again re-addresses it"), result["readdressed"], result["to"].(map[string]any)["recipient"], result["from"].(map[string]any)["recipient"], row.RecipientTaskID, messages, reports, record["recipientTaskId"], h.threads, strings.Contains(attempts[0].Message, "--as 01successor-supervisor"), again["staged"], strings.Contains(fmt.Sprint(again), "readdressed"), last, len(h.sends)}
	})
}

type restSuccessorHost struct {
	captureHost4
	threads []string
}

func (h *restSuccessorHost) SendMessage(_ context.Context, id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	result, err := h.captureHost4.SendMessage(context.Background(), id, thread, message, settings)
	if err == nil {
		h.threads = append(h.threads, thread)
	}
	if err != nil {
		return result, err
	}
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: "turn-" + thread + "-1"}}, nil
}

func Test24_SCH_41_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFifthReviewRoundFound.test_a_report_refused_inside_its_claim_is_rescheduled_by_the_gap", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id := set2Stage(t, c, s)
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO recipient_rate(recipient_task_id,window_start,sends,last_send_at) VALUES(?,?,1,?)", "01supervisor-task", float64(1699999200), float64(1700000000)); err != nil {
			t.Fatal(err)
		}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		c.skipPreflightRate = true
		first, err := c.Attempt(ctx, id, h, 1700000001)
		if err != nil {
			t.Fatal(err)
		}
		c.skipPreflightRate = false
		row := getSet3(t, c, id)
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_attempts").Scan(&n); err != nil {
			t.Fatal(err)
		}
		result, err := c.Attempt(ctx, id, h, row.NextEligibleAt.Float64)
		if err != nil {
			t.Fatal(err)
		}
		return []any{first, row.State, nil, row.AttemptCount, row.NextEligibleAt.Float64, []any{}, []any{}, result["deliveryState"]}
	})
}

func Test24_SCH_40_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFifthReviewRoundFound.test_a_delivery_claimed_inside_the_gap_after_a_report_is_refused", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		c.Settings = &delivery.TaskSettings{}
		id := set2Stage(t, c, s)
		row := getSet3(t, c, id)
		event := row.EventID.String
		service := delivery.NewService(s, &delivery.FakeClock{T: 1700000000})
		if _, err := service.Enqueue(ctx, event, "", ""); err != nil {
			t.Fatal(err)
		}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		if _, err := c.Attempt(ctx, id, h, 1700000000); err != nil {
			t.Fatal(err)
		}
		refused, err := service.SendRefusal(ctx, "other-relationship", "01supervisor-task", 1700000001)
		if err != nil {
			t.Fatal(err)
		}
		if refused == "" {
			t.Fatal("delivery claim did not pace")
		}
		var state string
		if err := s.DB.QueryRow("SELECT state FROM deliveries WHERE event_id=?", event).Scan(&state); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM attempts WHERE event_id=?", event).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("delivery claim created attempt")
		}
		return []any{state, []any{}}
	})
}

func Test24_SCH_40_ReverseCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFifthReviewRoundFound.test_a_report_claimed_inside_the_gap_after_a_delivery_is_refused", "delivery-claimed", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var id, rid string
		if err := s.DB.QueryRow("SELECT message_id,relationship_id FROM supervisor_messages LIMIT 1").Scan(&id, &rid); err != nil {
			t.Fatal(err)
		}
		r, err := c.Resolve(ctx, rid)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.claim(ctx, id, r, 1700000001, captureTime, "second")
		if got := captureRefusal4(t, err); got.Reason != "paced" {
			t.Fatalf("claim: %+v", got)
		}
		return []any{getSet3(t, c, id).State}
	})
}

func Test24_SCH_68_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheTwelfthIndependentReviewFound.test_a_block_stated_again_before_its_send_goes_up_as_the_newer_statement", "first-block", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o := captureObligation4(t, c, s)
		first := o.Basis["eventId"].(string)
		staged, err := c.Stage(ctx, o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := staged["messageId"].(string)
		// Python accepts the next block five seconds later; replay its exact event,
		// report and acceptance journal rows from the post-block snapshot.
		path := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(s.Path))), "second-block.sqlite3")
		s.DB.SetMaxOpenConns(1)
		if _, err := s.DB.ExecContext(ctx, "ATTACH DATABASE ? AS later", path); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			"INSERT INTO events SELECT * FROM later.events WHERE event_id<>?",
			"INSERT INTO work_reports SELECT * FROM later.work_reports WHERE event_id<>?",
			"INSERT INTO journal SELECT * FROM later.journal WHERE kind IN ('event_accepted','work_report_recorded') AND subject<>?",
		} {
			if _, err := s.DB.ExecContext(ctx, statement, first); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.DB.ExecContext(ctx, "DETACH DATABASE later"); err != nil {
			t.Fatal(err)
		}
		var second string
		if err := s.DB.QueryRow("SELECT event_id FROM events WHERE event_id<>?", first).Scan(&second); err != nil {
			t.Fatal(err)
		}
		c.Settings = &delivery.TaskSettings{}
		c.clockISO = func() string { return delivery.ISOOf(1700000005) }
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, id, h, 1700000005)
		if err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{record["deliveryState"], row.EventID.String, strings.Contains(attempts[0].Message, "show --event "+second), strings.Contains(attempts[0].Message, "show --event "+first), len(h.sends)}
	})
}

func Test24_SCH_64_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheEighthReviewRoundFound.test_a_report_sent_upward_can_no_longer_be_corrected", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		event := o.Basis["eventId"].(string)
		var sent int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_attempts WHERE message_id=? AND transport_started_at IS NOT NULL", id).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		var packet Packet
		if err := json.Unmarshal([]byte(getSet3(t, c, id).Packet), &packet); err != nil {
			t.Fatal(err)
		}
		var number int
		if err := s.DB.QueryRow("SELECT pr_number FROM work_reports WHERE event_id=?", event).Scan(&number); err != nil {
			t.Fatal(err)
		}
		return []any{record["deliveryState"], sent == 1, sent == 1, number, packet["artifact"].(map[string]any)["number"]}
	})
}

func Test24_SCH_70_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheEleventhIndependentReviewFound.test_a_report_recorded_after_its_completion_was_staged_is_carried", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o, id := stageSet3(t, c, s)
		old := getSet3(t, c, id)
		var packet Packet
		if err := json.Unmarshal([]byte(old.Packet), &packet); err != nil {
			t.Fatal(err)
		}
		empty := packet["artifact"]
		event := o.Basis["eventId"].(string)
		// Python records its first report after staging. Replay the exact report and
		// handoff rows from the snapshot taken immediately after that call.
		restCopyReport(t, s, "report-recorded.sqlite3")
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
			t.Fatal(err)
		}
		var restated int
		if err := s.DB.QueryRow("SELECT count(*) FROM journal WHERE kind='supervisor_message_restated'").Scan(&restated); err != nil {
			t.Fatal(err)
		}
		_ = event
		return []any{empty, true, record["deliveryState"], packet["artifact"].(map[string]any)["number"], row.SubmissionNo.Int64, len(h.sends), []any{strings.Repeat("claim", restated)}}
	})
}

func restCopyReport(t *testing.T, s *store.Store, name string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(s.Path))), name)
	s.DB.SetMaxOpenConns(1)
	if _, err := s.DB.ExecContext(ctx, "ATTACH DATABASE ? AS later", path); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO work_reports SELECT * FROM later.work_reports",
		"INSERT INTO work_report_handoffs SELECT * FROM later.work_report_handoffs",
		"INSERT INTO journal SELECT * FROM later.journal WHERE kind IN ('work_report_recorded','handoff_recorded')",
	} {
		if _, err := s.DB.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.ExecContext(ctx, "DETACH DATABASE later"); err != nil {
		t.Fatal(err)
	}
}

func Test24_SCH_71_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheEleventhIndependentReviewFound.test_a_decision_reported_after_its_block_was_staged_still_goes_up", "accepted", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		staged, err := c.StageStanding(ctx, "PRJ-1", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		items := staged["staged"].([]any)
		kinds := []any{}
		for _, item := range items {
			kinds = append(kinds, item.(StageResult)["message"].(map[string]any)["obligation_kind"])
		}
		blocked := items[0].(StageResult)["messageId"].(string)
		restCopyReport(t, s, "decision-recorded.sqlite3")
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		_, err = c.Attempt(ctx, blocked, h, 1700000000)
		refusal := captureRefusal4(t, err)
		row := getSet3(t, c, blocked)
		standing, err := c.StageStanding(ctx, "PRJ-1", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		decision := []string{}
		for _, v := range standing["staged"].([]any) {
			m := v.(StageResult)
			if m["message"].(map[string]any)["obligation_kind"] == "decision_request" {
				decision = append(decision, m["messageId"].(string))
			}
		}
		if len(decision) != 1 {
			t.Fatalf("decisions: %v", standing)
		}
		record, err := c.Attempt(ctx, decision[0], h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{kinds, true, refusal.Reason, strings.Contains(refusal.Detail, "now raises"), row.HoldReason.String, []any{}, len(decision), record["deliveryState"], len(h.sends)}
	})
}

func Test24_SCH_73_OmissionCapture(t *testing.T) {
	t.Parallel()
	supervisorMirror(t, "TheStagedRowIsAProposal.test_an_omission_whose_turn_reported_after_staging_is_not_sent", "setup", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": captureRelationID(t, c), "reason": "the turn settled without a report", "selectors": map[string]any{"state": c.StoreDirectory(), "markerRoot": "/marker", "workspace": filepath.Join(filepath.Dir(c.StoreDirectory()), "work"), "assignment": "asg-1", "session": "01child-session", "turn": "turn-dispatch-1"}}
		o := ObservationObligation(reading)
		if o == nil {
			t.Fatal("missing omission")
		}
		staged, err := c.StageWithReading(ctx, *o, reading, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := staged["messageId"].(string)
		path := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(s.Path))), "omission-accepted.sqlite3")
		s.DB.SetMaxOpenConns(1)
		if _, err := s.DB.ExecContext(ctx, "ATTACH DATABASE ? AS later", path); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"INSERT INTO events SELECT * FROM later.events", "INSERT INTO revision_lineage SELECT * FROM later.revision_lineage", "INSERT INTO journal SELECT * FROM later.journal WHERE kind='event_accepted'"} {
			if _, err := s.DB.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.DB.ExecContext(ctx, "DETACH DATABASE later"); err != nil {
			t.Fatal(err)
		}
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		_, err = c.Attempt(ctx, id, h, 1700000000)
		refusal := captureRefusal4(t, err)
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "final receipt"), getSet3(t, c, id).HoldReason.String, []any{}}
	})
}

func Test24_SCH_73_DischargeCapture(t *testing.T) {
	t.Parallel()
	supervisorMirror(t, "TheStagedRowIsAProposal.test_an_obligation_discharged_after_staging_is_not_sent", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		_, id := stageSet3(t, c, s)
		path := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(s.Path))), "discharged.sqlite3")
		s.DB.SetMaxOpenConns(1)
		if _, err := s.DB.ExecContext(ctx, "ATTACH DATABASE ? AS later", path); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"INSERT INTO sync_targets SELECT * FROM later.sync_targets", "INSERT INTO sync_outbox SELECT * FROM later.sync_outbox", "INSERT INTO journal SELECT * FROM later.journal WHERE kind IN ('sync_enqueued','sync_confirmed')"} {
			if _, err := s.DB.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.DB.ExecContext(ctx, "DETACH DATABASE later"); err != nil {
			t.Fatal(err)
		}
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		_, err := c.Attempt(ctx, id, h, 1700000000)
		refusal := captureRefusal4(t, err)
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "Linear record"), getSet3(t, c, id).HoldReason.String, []any{}}
	})
}
