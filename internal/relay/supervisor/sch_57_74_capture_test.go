package supervisor

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const captureAt57 = "2023-11-14T22:13:20.000000+00:00"

func captureStage57(t *testing.T, c *Channel, s *store.Store) (Obligation, string) {
	t.Helper()
	var event string
	if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
		t.Fatal(err)
	}
	o, err := c.FromEvent(context.Background(), event)
	if err != nil || o == nil {
		t.Fatalf("obligation %v %v", o, err)
	}
	staged, err := c.Stage(context.Background(), *o, "", captureAt57)
	if err != nil {
		t.Fatal(err)
	}
	return *o, staged["messageId"].(string)
}

type captureHost57 struct{ *sendHost }

func (h *captureHost57) ReadGoalStatus(context.Context, string) (any, error) { return nil, nil }
func (h *captureHost57) ReadTurn(_ context.Context, _ string, id string) (*delivery.TurnInfo, error) {
	if id == "turn-01supervisor-task-1" {
		at := float64(1700000000)
		return &delivery.TurnInfo{TurnID: id, StartedAt: &at}, nil
	}
	return h.sendHost.ReadTurn(context.Background(), "", id)
}
func (h *captureHost57) SendMessage(_ context.Context, id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	result, err := h.sendHost.SendMessage(context.Background(), id, thread, message, settings)
	if err != nil {
		return result, err
	}
	turn := "turn-" + thread + "-1"
	h.items[turn] = message
	delete(h.items, "turn-supervisor-1")
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: turn}}, nil
}
func captureSend57(t *testing.T, c *Channel, id string, h SendAdapter, at float64) map[string]any {
	t.Helper()
	c.Settings = &delivery.TaskSettings{}
	previous := TokenSource
	TokenSource = bytes.NewReader(make([]byte, 8))
	t.Cleanup(func() { TokenSource = previous })
	result, err := c.Attempt(context.Background(), id, h, at)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func Test24_SCH_57_LiveTransportStarted(t *testing.T) {
	supervisorMirror(t, "WhatTheEighthReviewRoundFound.test_the_full_record_shows_when_each_transport_started", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		captureSend57(t, c, id, &captureHost57{&sendHost{status: "idle"}}, 1700000000)
		shown, err := c.Show(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		attempts := shown["attempts"].([]any)
		return []any{attempts[0].(map[string]any)["transportStartedAt"] != nil}
	})
}
func Test24_SCH_58_LiveExpiredOwner(t *testing.T) {
	supervisorMirror(t, "ASettlementBelongsToTheClaimThatMadeIt.test_an_expired_lease_nobody_recovered_still_belongs_to_its_claim", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		h := &captureHost57{&sendHost{status: "idle"}}
		h.beforeSend = func() { c.clockISO = func() string { return delivery.ISOOf(1700000301) } }
		record := captureSend57(t, c, id, h, 1700000000)
		row, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{record["deliveryState"], row.State}
	})
}
func Test24_SCH_58_LiveOwningClaim(t *testing.T) {
	supervisorMirror(t, "ASettlementBelongsToTheClaimThatMadeIt.test_the_claim_that_holds_the_message_settles_it", "event", func(c *Channel, s *store.Store) []any {
		_, id := captureStage57(t, c, s)
		record := captureSend57(t, c, id, &captureHost57{&sendHost{status: "idle"}}, 1700000000)
		row, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{record["deliveryState"], row.State}
	})
}
func Test24_SCH_63_LiveArrivalOnly(t *testing.T) {
	supervisorMirror(t, "EveryLineSelectsTheStoreItWasWrittenFrom.test_the_request_asks_only_for_what_a_readback_records", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		_, id := captureStage57(t, c, s)
		h := &captureHost57{&sendHost{status: "idle"}}
		record := captureSend57(t, c, id, h, 1700000000)
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		written := attempts[0].Message
		turn := record["turnId"].(string)
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		shown, err := c.Show(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		reach, err := c.Reach(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{strings.Contains("confirm you read", strings.ToLower(written)), strings.Contains(written, "It never records that you read"), answer["turnOrigin"], strings.HasPrefix(answer["establishes"].(string), "arrival only"), []any{shown["state"], shown["turnOrigin"]}, strings.HasPrefix(shown["readEstablishes"].(string), "arrival only"), shown["readback"].(map[string]any)["turnOrigin"], strings.Contains(reach["received"].(map[string]any)["detail"].(string), "relay_opened")}
	})
}

func Test24_SCH_61_LiveOwedWithoutStaging(t *testing.T) {
	supervisorMirror(t, "AParentThatNeverReports.test_a_parent_that_never_stages_or_sends_still_reads_the_report_as_owed", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var event string
		if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation %v %v", o, err)
		}
		standing, err := c.Standing(ctx, "PRJ-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		owed, _ := standing["standing"].([]any)
		matching := []any{}
		for _, raw := range owed {
			entry, _ := raw.(map[string]any)
			basis, _ := entry["basis"].(map[string]any)
			if basis["eventId"] == event {
				matching = append(matching, entry)
			}
		}
		entry := matching[0].(map[string]any)
		decision := entry["decision"].(map[string]any)
		refused := standing["refused"]
		if refused == nil {
			refused = []any{}
		}
		var messageIDs, journalIDs []any
		for _, query := range []struct {
			sql string
			out *[]any
		}{{"SELECT message_id FROM supervisor_messages", &messageIDs}, {"SELECT seq FROM journal WHERE kind='supervisor_report'", &journalIDs}} {
			rows, err := s.DB.Query(query.sql)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var value any
				if err := rows.Scan(&value); err != nil {
					t.Fatal(err)
				}
				*query.out = append(*query.out, value)
			}
			rows.Close()
		}
		if messageIDs == nil {
			messageIDs = []any{}
		}
		if journalIDs == nil {
			journalIDs = []any{}
		}
		return []any{len(matching), entry["kind"], decision["standing"], decision["report"], decision["priorReport"], messageIDs, journalIDs}
	})
}

func Test24_SCH_60_LiveOtherStore(t *testing.T) {
	supervisorMirror(t, "WhatTheSeventhIndependentReviewFound.test_an_omission_read_against_another_store_is_refused", "setup", func(c *Channel, s *store.Store) []any {
		reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": captureRelationID(t, c), "executionGeneration": float64(1), "reason": "missing", "selectors": map[string]any{"state": filepath.Join(c.StoreDirectory(), "another-store"), "markerRoot": "/tmp/markers", "workspace": "/tmp/work", "assignment": "a", "session": "s", "turn": "turn-1"}}
		o := ObservationObligation(reading)
		if o == nil {
			t.Fatal("no obligation")
		}
		_, err := c.StageWithReading(context.Background(), *o, reading, "", captureAt57)
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("expected refusal: %v", err)
		}
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "another store"), []any{}}
	})
}

func Test24_SCH_59_LiveAlteredObligation(t *testing.T) {
	supervisorMirror(t, "WhatTheFifthIndependentReviewFound.test_an_obligation_its_caller_altered_is_refused", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var event string
		if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation %v %v", o, err)
		}
		values := []any{}
		for _, field := range []string{"executionGeneration", "issueKey"} {
			changed := *o
			if field == "executionGeneration" {
				n := int64(99)
				changed.Generation = &n
			} else {
				issue := "OTHER-1"
				changed.Issue = &issue
			}
			_, err = c.Stage(ctx, changed, "", captureAt57)
			var refusal Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("expected refusal: %v", err)
			}
			values = append(values, refusal.Reason, strings.Contains(refusal.Detail, field))
		}
		for _, query := range []string{"SELECT message_id FROM supervisor_messages", "SELECT seq FROM journal WHERE kind='supervisor_report'"} {
			rows, err := s.DB.Query(query)
			if err != nil {
				t.Fatal(err)
			}
			ids := []any{}
			for rows.Next() {
				var id any
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			rows.Close()
			values = append(values, ids)
		}
		return values
	})
}
