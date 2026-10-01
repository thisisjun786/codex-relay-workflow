package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func stageSet3(t *testing.T, c *Channel, s *store.Store) (Obligation, string) {
	t.Helper()
	o := captureObligation4(t, c, s)
	result, err := c.Stage(context.Background(), o, "", captureTime)
	if err != nil {
		t.Fatal(err)
	}
	return o, result["messageId"].(string)
}
func sendSet3(t *testing.T, c *Channel, s *store.Store) (string, *captureHost4, map[string]any) {
	t.Helper()
	captureTokens4(t)
	h := &captureHost4{sendHost: sendHost{status: "idle"}}
	id, result := captureSend4(t, c, s, h)
	return id, h, result
}
func getSet3(t *testing.T, c *Channel, id string) store.SupervisorMessagesRow {
	t.Helper()
	row, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
func handoverSet3(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	original, successor := "bnd-24179d1961baacd1886d337c38c3eceb", "bnd-b5f442e2faff9adf98768ffc7139b265"
	if err := storeseed.ArchiveScopeBinding(ctx, s, original, "archived", successor, captureTime); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(ctx, s, store.ScopeBindingsRow{BindingID: successor, Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "01successor-supervisor", HostID: "host-a", Status: "active", Revision: 2, CreatedAt: captureTime, UpdatedAt: captureTime, CWD: sql.NullString{String: "/successor", Valid: true}, CXCSession: sql.NullString{String: "cxc-next", Valid: true}, HandoverNote: sql.NullString{String: "the initiative changed hands", Valid: true}, Supersedes: sql.NullString{String: original, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ kind, detail string }{{"scope_bound", `{"role": "supervisor", "scopeKind": "initiative", "scopeKey": "INI-1", "taskId": "01successor-supervisor", "revision": 2}`}, {"scope_handover", `{"scopeKey": "INI-1", "from": "01supervisor-task", "to": "01successor-supervisor", "actor": "a test", "acknowledged": []}`}} {
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureTime, entry.kind, successor, entry.detail); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeseed.RepointScopeLink(ctx, s, "lnk-6fa68afd8cc27a8e7780d400a52f73c2", "active", "01parent-task", "01successor-supervisor", captureTime); err != nil {
		t.Fatal(err)
	}
}

type killedHostSet3 struct{ *captureHost4 }

func (h *killedHostSet3) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	_, err := h.captureHost4.SendMessage(id, thread, message, settings)
	if err != nil {
		return nil, err
	}
	panic("worker killed")
}
func Test24_SCH_43_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_late_receipt_does_not_drag_a_settled_message_back", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		captureTokens4(t)
		_, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		// The host accepted the bytes but its sender died before recording a receipt.
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		func() {
			defer func() {
				if r := recover(); r != "worker killed" {
					t.Fatalf("expected killed worker, got %v", r)
				}
			}()
			_, _ = c.Attempt(ctx, id, &killedHostSet3{h}, 1700000000)
		}()
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		values := []any{attempts[0].TransportStartedAt.Valid, getSet3(t, c, id).State}
		c.clockISO = func() string { return delivery.ISOOf(1700000301) }
		if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_attempts SET record=? WHERE request_id=?", pyjson.Dumps(map[string]any{"requestId": attempts[0].RequestID}, pyjson.Options{SortKeys: true, Unicode: true}), attempts[0].RequestID); err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		if _, err := c.recoverStranded(ctx, row, 1700000301); err != nil {
			t.Fatal(err)
		}
		turn := "turn-01supervisor-task-1"
		_, err = c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000301)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, getSet3(t, c, id).State)
		if err := s.SettleSupervisorAttempt(ctx, attempts[0].RequestID, "dispatched", "yes", 0, sql.NullString{String: turn, Valid: true}, `{"requestId": "`+attempts[0].RequestID+`"}`, delivery.ISOOf(1700000301)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_attempted',?,?)", delivery.ISOOf(1700000301), id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: attempts[0].RequestID}, {Key: "attemptNo", Value: 1}, {Key: "deliveryState", Value: "dispatched"}, {Key: "sendAttempted", Value: "yes"}, {Key: "turnId", Value: turn}, {Key: "holdReason", Value: nil}, {Key: "messageMoved", Value: false}, {Key: "messageState", Value: "read"}, {Key: "reason", Value: "this claim no longer held the message when its receipt arrived, so the receipt is recorded on its attempt and the message is left where it is"}}, pyjson.Options{})); err != nil {
			t.Fatal(err)
		}
		return append(values, getSet3(t, c, id).State, "dispatched")
	})
}
func Test24_SCH_44_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_report_recorded_between_the_reading_and_the_write_is_not_staged", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o := captureObligation4(t, c, s)
		// The concurrent report is committed before the staging transaction opens.
		note := "a concurrent supervisor-report-recorded"
		_, err := c.RecordReport(ctx, o, captureTime, nil, &note)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Stage(ctx, o, "", captureTime)
		r := captureRefusal4(t, err)
		var n int
		s.DB.QueryRow(`SELECT count(*) FROM journal WHERE kind='supervisor_report'`).Scan(&n)
		return []any{r.Reason, strings.Contains(r.Detail, "decided under the staging write lock"), []any{}, n}
	})
}
func Test24_SCH_45_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_stage_from_the_former_hierarchy_landing_first_is_readdressed", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		one, id := stageSet3(t, c, s)
		row := getSet3(t, c, id)
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM supervisor_messages WHERE message_id=?", id); err != nil {
			t.Fatal(err)
		}
		handoverSet3(t, s)
		// A caller holding the frozen pre-handover row commits immediately before staging.
		columns := []string{"message_id", "obligation_id", "obligation_kind", "relationship_id", "event_id", "subject", "sender_task_id", "recipient_task_id", "project_key", "purpose", "kind", "packet", "reading", "state", "hold_reason", "next_eligible_at", "lease_owner", "lease_until", "attempt_count", "submission_no", "staged_at", "updated_at"}
		values := []any{row.MessageID, row.ObligationID, row.ObligationKind, row.RelationshipID, row.EventID, row.Subject, row.SenderTaskID, row.RecipientTaskID, row.ProjectKey, row.Purpose, row.Kind, row.Packet, row.Reading, row.State, row.HoldReason, row.NextEligibleAt, row.LeaseOwner, row.LeaseUntil, row.AttemptCount, row.SubmissionNo, row.StagedAt, row.UpdatedAt}
		for i, v := range values {
			switch x := v.(type) {
			case sql.NullString:
				if x.Valid {
					values[i] = x.String
				} else {
					values[i] = nil
				}
			case sql.NullInt64:
				if x.Valid {
					values[i] = x.Int64
				} else {
					values[i] = nil
				}
			case sql.NullFloat64:
				if x.Valid {
					values[i] = x.Float64
				} else {
					values[i] = nil
				}
			}
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_messages ("+strings.Join(columns, ",")+") VALUES ("+placeholders+")", values...); err != nil {
			t.Fatal(err)
		}
		answer, err := c.Stage(ctx, one, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM journal WHERE kind='supervisor_report' AND subject=?", one.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return []any{answer["readdressed"], getSet3(t, c, id).RecipientTaskID, n}
	})
}
func Test24_SCH_46_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_stale_reschedule_does_not_clear_a_hold_another_caller_set", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		_, id := stageSet3(t, c, s)
		stale := getSet3(t, c, id)
		if _, err := s.DB.ExecContext(ctx, `UPDATE supervisor_messages SET state='deferred_busy',hold_reason='busy_cap' WHERE message_id=?`, id); err != nil {
			t.Fatal(err)
		}
		if err := c.deferMessage(ctx, stale, "queued", "", captureTime, 1700000005); err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		return []any{[]any{row.State, row.HoldReason.String}}
	})
}
func Test24_SCH_46_FormerRecipientCapture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_stale_reschedule_does_not_put_the_former_recipients_delay_back", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		one, id := stageSet3(t, c, s)
		stale := getSet3(t, c, id)
		handoverSet3(t, s)
		_, err := c.Stage(ctx, one, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.deferMessage(ctx, stale, "queued", "", captureTime, 1700000600); err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		if row.NextEligibleAt.Valid {
			t.Fatalf("stale reschedule set delay: %v", row.NextEligibleAt)
		}
		return []any{row.RecipientTaskID, nil}
	})
}
func Test24_SCH_48_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_turn_opened_between_the_claim_and_the_transport_does_not_verify", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		captureTokens4(t)
		_, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		c.beforeTransport = func() { c.clockISO = func() string { return delivery.ISOOf(1700000010) } }
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		turn := record["turnId"].(string)
		h.turns = map[string]float64{turn: 1700000005}
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000010)
		if err != nil {
			t.Fatal(err)
		}
		return []any{turn, answer["verified"], getSet3(t, c, id).State}
	})
}
func Test24_SCH_47_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_send_dated_ahead_by_a_fast_clock_does_not_stall_the_recipient", "setup", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		ahead := float64(1700086400)
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO recipient_rate(recipient_task_id,window_start,sends,last_send_at) VALUES(?,?,1,?)", "01supervisor-task", math.Floor(ahead/3600)*3600, ahead); err != nil {
			t.Fatal(err)
		}
		service := delivery.NewService(s, delivery.SystemClock{})
		reason, err := service.SendRefusal(ctx, "01supervisor-task", 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		if reason != "" {
			t.Fatalf("unexpected refusal: %s", reason)
		}
		return []any{nil}
	})
}
func Test24_SCH_49_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSeventhReviewRoundFound.test_a_token_in_no_named_turn_does_not_verify", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id, h, result := sendSet3(t, c, s)
		h.items = map[string]string{"": h.sends[0]}
		turn := result["turnId"].(string)
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{answer["verified"], strings.Contains(answer["detail"].(string), "did not say which turn"), getSet3(t, c, id).State}
	})
}
func Test24_SCH_48_QueuedTransportCapture(t *testing.T) {
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_a_turn_opened_while_the_transport_queued_the_message_does_not_verify", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		captureTokens4(t)
		_, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}, steered: true}
		h.beforeSend = func() { c.clockISO = func() string { return delivery.ISOOf(1700000010) } }
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		at := delivery.ISOOf(1700000010)
		if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_attempts SET observed_at=?,record=? WHERE message_id=?", at, pyjson.Dumps(map[string]any{"schema": channelVersion, "requestId": record["requestId"], "messageId": id, "attemptNo": 1, "recipientTaskId": "01supervisor-task", "deliveryState": "dispatched", "sendAttempted": "yes", "retrySafe": false, "transportReceiptStatus": "accepted", "failedOperation": nil, "turnId": record["turnId"], "observedAt": at}, pyjson.Options{SortKeys: true, Unicode: true}), id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE journal SET at=? WHERE kind='supervisor_message_attempted' AND subject=?", at, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET updated_at=? WHERE message_id=?", at, id); err != nil {
			t.Fatal(err)
		}
		turn := "turn-01supervisor-task-1"
		h.turns = map[string]float64{turn: 1700000005, "turn-01supervisor-task-2": 1700000010}
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000010)
		if err != nil {
			t.Fatal(err)
		}
		return []any{record["turnId"], answer["verified"], strings.Contains(answer["detail"].(string), record["turnId"].(string)), getSet3(t, c, id).State}
	})
}

type invalidLandingSet3 struct {
	*captureHost4
	landing string
}

func (h *invalidLandingSet3) ReadTurn(thread, id string) (*delivery.TurnInfo, error) {
	if id == h.landing {
		v := math.NaN()
		return &delivery.TurnInfo{TurnID: id, StartedAt: &v}, nil
	}
	return h.captureHost4.ReadTurn(thread, id)
}
func Test24_SCH_52_LandingCapture(t *testing.T) {
	supervisorMirror(t, "WhatTheThirdIndependentReviewFound.test_a_landing_turn_whose_start_is_not_a_time_does_not_verify", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id, h, record := sendSet3(t, c, s)
		c.clockISO = func() string { return delivery.ISOOf(1700000030) }
		own := "turn-01supervisor-task-2"
		h.turns = map[string]float64{own: 1700000030}
		answer, err := c.ReadBack(ctx, id, own, Proof(id, own), "", &invalidLandingSet3{h, record["turnId"].(string)}, 1700000030)
		if err != nil {
			t.Fatal(err)
		}
		return []any{answer["turnOrigin"], answer["verified"], getSet3(t, c, id).State}
	})
}
func Test24_SCH_52_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheThirdIndependentReviewFound.test_a_turn_start_that_is_not_a_time_does_not_verify", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id, h, result := sendSet3(t, c, s)
		turn := result["turnId"].(string)
		values := []any{}
		for _, start := range []*float64{nil, func() *float64 { v := math.NaN(); return &v }(), func() *float64 { v := math.Inf(1); return &v }(), nil, nil} {
			host := &turnStartHost{sendHost: &h.sendHost, started: start}
			answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", host, 1700000000)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, answer["verified"], getSet3(t, c, id).State)
		}
		return values
	})
}

type captureSuccessorSet3 struct{ *captureHost4 }

func (h *captureSuccessorSet3) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	return h.accept(id, "turn-"+thread+"-1", message, settings), nil
}
func Test24_SCH_51_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheThirdIndependentReviewFound.test_a_handover_after_the_claim_and_before_the_transport_sends_nothing", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		captureTokens4(t)
		one, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		c.beforeTransport = func() { handoverSet3(t, s) }
		_, err := c.Attempt(ctx, id, h, 1700000000)
		r := captureRefusal4(t, err)
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		values := []any{r.Reason, strings.Contains(r.Detail, "Nothing was sent"), []any{}, []any{attempts[0].SendAttempted, attempts[0].RetrySafe, nil}}
		moved, err := c.Stage(ctx, one, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, moved["readdressed"])
		c.beforeTransport = nil
		TokenSource = bytes.NewReader([]byte{0, 0, 0, 0, 0, 0, 0, 1})
		successorHost := &captureSuccessorSet3{captureHost4: h}
		record, err := c.Attempt(ctx, id, successorHost, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, record["recipientTaskId"], []any{"01successor-supervisor"})
		return values
	})
}

type unknownHostSet3 struct{ *captureHost4 }

func (h *unknownHostSet3) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.accept(id, "turn-01supervisor-task-1", message, settings)
	return delivery.Obj{{Key: "status", Value: delivery.OutcomeUnknown}, {Key: "requestId", Value: id}}, nil
}
func Test24_SCH_50_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheSeventhReviewRoundFound.test_every_answer_to_one_readback_has_the_shape_of_the_first", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		captureTokens4(t)
		_, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle", outcome: "unknown"}}
		record, err := c.Attempt(ctx, id, &unknownHostSet3{h}, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		values := []any{record["deliveryState"], getSet3(t, c, id).State}
		c.clockISO = func() string { return delivery.ISOOf(1700000001) }
		turn := "turn-01supervisor-task-1"
		h.turns = map[string]float64{turn: 1700000001}
		first, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000001)
		if err != nil {
			t.Fatal(err)
		}
		settled, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000001)
		if err != nil {
			t.Fatal(err)
		}
		raced := readAnswer(func() store.SupervisorReadbacksRow {
			row, e := s.SupervisorReadback(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			return row
		}(), false, true)
		values = append(values, []any{first["recorded"], settled["recorded"], raced["recorded"]}, []any{first["raced"], settled["raced"], raced["raced"]})
		for _, later := range []map[string]any{settled, raced} {
			keys := make([]string, 0, len(later))
			for key := range later {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			values = append(values, keys)
			for _, key := range []string{"schema", "messageId", "verified", "readTurnId", "turnOrigin", "detail", "delivered", "readAt", "assertedBy", "reconciled", "establishes", "limits"} {
				values = append(values, later[key])
			}
		}
		if _, ok := settled["detail"].(string); !ok {
			t.Fatal("detail is not a string")
		}
		values = append(values, settled["reconciled"].(map[string]any)["requestId"])
		return values
	})
}
func Test24_SCH_53_Capture(t *testing.T) {
	for _, tc := range []struct {
		name, id   string
		submission int64
	}{{"corrected", "WhatTheEighthReviewRoundFound.test_a_staged_report_corrected_in_place_before_the_send_goes_up_corrected", 1}, {"resubmitted", "WhatTheEighthReviewRoundFound.test_a_staged_report_resubmitted_before_the_send_goes_up_resubmitted", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			supervisorMirror(t, tc.id, "event", func(c *Channel, s *store.Store) []any {
				ctx := context.Background()
				captureTokens4(t)
				one, id := stageSet3(t, c, s)
				event := one.Basis["eventId"].(string)
				row := getSet3(t, c, id)
				var oldPacket Packet
				if err := json.Unmarshal([]byte(row.Packet), &oldPacket); err != nil {
					t.Fatal(err)
				}
				values := []any{oldPacket["artifact"].(map[string]any)["number"]}
				head := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345611"
				if tc.submission == 1 {
					if _, err := s.DB.ExecContext(ctx, "UPDATE work_reports SET pr_number=11,pr_url=?,head_sha=? WHERE event_id=? AND submission_no=1", "https://github.com/thisisjun786/codex-relay-workflow/pull/11", head, event); err != nil {
						t.Fatal(err)
					}
					if _, err := s.DB.ExecContext(ctx, "UPDATE work_report_handoffs SET checks=replace(checks,?,?) WHERE event_id=? AND submission_no=1", "a1b2c3d4e5f60718293a4b5c6d7e8f9012345610", head, event); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := s.DB.ExecContext(ctx, "INSERT INTO work_reports SELECT event_id,2,relationship_id,execution_generation,revision_hash,repository,11,?,pr_state,base_ref,base_sha,?,criteria_digest,cxc_status,cxc_reason,contract_version,summary,evidence,unresolved,next_action,review,restore,recorded_at FROM work_reports WHERE event_id=? AND submission_no=1", "https://github.com/thisisjun786/codex-relay-workflow/pull/11", head, event); err != nil {
						t.Fatal(err)
					}
					if _, err := s.DB.ExecContext(ctx, "INSERT INTO work_report_handoffs SELECT event_id,2,is_draft,base_verified_at,required_declared,replace(checks,?,?),review_coverage,thread_dispositions,criterion_evidence,limitations,recorded_at FROM work_report_handoffs WHERE event_id=? AND submission_no=1", "a1b2c3d4e5f60718293a4b5c6d7e8f9012345610", head, event); err != nil {
						t.Fatal(err)
					}
				}
				for _, entry := range []struct{ kind, detail string }{{"work_report_recorded", `{"repository": "thisisjun786/codex-relay-workflow", "prNumber": 11, "cxcStatus": "DONE", "headSha": "` + head + `", "submissionNo": ` + strconv.FormatInt(tc.submission, 10) + `}`}, {"handoff_recorded", `{"headSha": "` + head + `", "prNumber": 11, "threadsSeen": 1, "submissionNo": ` + strconv.FormatInt(tc.submission, 10) + `}`}} {
					if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureTime, entry.kind, event, entry.detail); err != nil {
						t.Fatal(err)
					}
				}
				c.Settings = &delivery.TaskSettings{}
				h := &captureHost4{sendHost: sendHost{status: "idle"}}
				record, err := c.Attempt(ctx, id, h, 1700000000)
				if err != nil {
					t.Fatal(err)
				}
				values = append(values, record["deliveryState"])
				updated := getSet3(t, c, id)
				var packet Packet
				if err := json.Unmarshal([]byte(updated.Packet), &packet); err != nil {
					t.Fatal(err)
				}
				values = append(values, packet["artifact"].(map[string]any)["number"], strings.Contains(h.sends[0], "codex-relay-workflow #11 at"))
				var number, n int
				if err := s.DB.QueryRow("SELECT pr_number FROM work_reports WHERE event_id=? ORDER BY submission_no DESC LIMIT 1", event).Scan(&number); err != nil {
					t.Fatal(err)
				}
				if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_attempts WHERE message_id=?", id).Scan(&n); err != nil {
					t.Fatal(err)
				}
				values = append(values, number, n)
				return values
			})
		})
	}
}
func Test24_SCH_53_SentFrozenCapture(t *testing.T) {
	supervisorMirror(t, "WhatTheEighthReviewRoundFound.test_a_report_sent_upward_can_no_longer_be_corrected", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		captureTokens4(t)
		one, id := stageSet3(t, c, s)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		values := []any{record["deliveryState"]}
		event := one.Basis["eventId"].(string)
		for range []int{1, 2} {
			var sent int
			if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM supervisor_messages m WHERE m.event_id=? AND EXISTS (SELECT 1 FROM supervisor_attempts a WHERE a.message_id=m.message_id AND a.transport_started_at IS NOT NULL AND NOT (a.send_attempted='no' AND a.retry_safe=1))", one.Basis["eventId"]).Scan(&sent); err != nil {
				t.Fatal(err)
			}
			if sent != 1 {
				t.Fatal("correction should be refused after transport start")
			}
			values = append(values, true)
		}
		var number int
		if err := s.DB.QueryRowContext(ctx, "SELECT pr_number FROM work_reports WHERE event_id=? ORDER BY submission_no DESC LIMIT 1", event).Scan(&number); err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, id)
		var packet Packet
		if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
			t.Fatal(err)
		}
		return append(values, number, packet["artifact"].(map[string]any)["number"])
	})
}
func Test24_SCH_54_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheEighthReviewRoundFound.test_a_correction_landing_before_the_staging_lock_refuses_the_stale_packet", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		one := captureObligation4(t, c, s)
		event := one.Basis["eventId"].(string)
		// The event snapshot already contains the concurrent correction. Remove it
		// for the pre-lock read and replay the same rows at the staging write seam.
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM work_reports WHERE event_id=? AND submission_no=2", event); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM journal WHERE kind IN ('work_report_recorded','handoff_recorded') AND json_extract(detail,'$.submissionNo')=2"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name='journal'"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO sqlite_sequence(name,seq) VALUES('journal',12)"); err != nil {
			t.Fatal(err)
		}
		c.beforeStageLock = func() {
			head := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345611"
			if _, err := s.DB.ExecContext(ctx, "INSERT INTO work_reports SELECT event_id,2,relationship_id,execution_generation,revision_hash,repository,11,?,pr_state,base_ref,base_sha,?,criteria_digest,cxc_status,cxc_reason,contract_version,summary,evidence,unresolved,next_action,review,restore,recorded_at FROM work_reports WHERE event_id=? AND submission_no=1", "https://github.com/thisisjun786/codex-relay-workflow/pull/11", head, event); err != nil {
				t.Fatal(err)
			}
			for _, entry := range []struct{ kind, detail string }{{"work_report_recorded", `{"repository": "thisisjun786/codex-relay-workflow", "prNumber": 11, "cxcStatus": "DONE", "headSha": "` + head + `", "submissionNo": 2}`}, {"handoff_recorded", `{"headSha": "` + head + `", "prNumber": 11, "threadsSeen": 1, "submissionNo": 2}`}} {
				if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureTime, entry.kind, event, entry.detail); err != nil {
					t.Fatal(err)
				}
			}
		}
		_, err := c.Stage(ctx, one, "", captureTime)
		c.beforeStageLock = nil
		r := captureRefusal4(t, err)
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_messages").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("stale packet staged: %d", n)
		}
		updated := captureObligation4(t, c, s)
		staged, err := c.Stage(ctx, updated, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		row := getSet3(t, c, staged["messageId"].(string))
		var packet Packet
		if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
			t.Fatal(err)
		}
		return []any{r.Reason, strings.Contains(r.Detail, "changed while this was being staged"), []any{}, packet["artifact"].(map[string]any)["number"], row.SubmissionNo.Int64}
	})
}
func Test24_SCH_55_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheEighthReviewRoundFound.test_an_omission_is_refused_with_a_reading_that_is_not_its_own", "setup", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		base := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": captureRelationID(t, c), "reason": "the turn settled without a report", "selectors": map[string]any{"state": c.StoreDirectory(), "markerRoot": "/marker", "workspace": strings.TrimSuffix(c.StoreDirectory(), "/state") + "/work", "assignment": "asg-1", "session": "01child-session", "turn": "turn-unreported-1"}}
		o := ObservationObligation(base)
		if o == nil {
			t.Fatal("no obligation")
		}
		values := []any{}
		for _, name := range []string{"schema", "state", "relationship", "turn"} {
			raw, _ := json.Marshal(base)
			var wrong map[string]any
			json.Unmarshal(raw, &wrong)
			switch name {
			case "schema":
				wrong["schema"] = "reporting-observation/0"
			case "state":
				wrong["reportingState"] = "reported"
			case "relationship":
				wrong["relationshipId"] = "rel-someone-else"
			case "turn":
				wrong["selectors"].(map[string]any)["turn"] = "turn-another"
			}
			_, err := c.StageWithReading(ctx, *o, wrong, "", captureTime)
			r := captureRefusal4(t, err)
			values = append(values, r.Reason, strings.Contains(r.Detail, "contradicts it"))
		}
		values = append(values, []any{}, []any{})
		staged, err := c.StageWithReading(ctx, *o, base, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, staged["staged"])
		return values
	})
}
