package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func set2Stage(t *testing.T, c *Channel, s *store.Store) string {
	t.Helper()
	c.clockISO = func() string { return captureTime }
	c.Settings = &delivery.TaskSettings{}
	o := captureObligation4(t, c, s)
	result, err := c.Stage(context.Background(), o, "", captureTime)
	if err != nil {
		t.Fatal(err)
	}
	return result["messageId"].(string)
}

func Test24_SCH_31_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheSecondReviewRoundFound.test_a_recipient_that_recovers_releases_the_report_it_was_holding", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id := set2Stage(t, c, s)
		h := &captureHost4{sendHost: sendHost{status: "idle", archived: true}}
		first, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		h.archived = false
		record, err := c.Attempt(ctx, id, h, row.NextEligibleAt.Float64)
		if err != nil {
			t.Fatal(err)
		}
		return []any{first, row.State, nil, record != nil, record["deliveryState"]}
	})
}

func Test24_SCH_30_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheMergeBoundaryReviewFound.test_a_turn_the_send_steered_rather_than_opened_does_not_verify", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		c.clockISO = func() string { return delivery.ISOOf(1700000600) }
		c.Settings = &delivery.TaskSettings{}
		o := captureObligation4(t, c, s)
		result, err := c.Stage(ctx, o, "", delivery.ISOOf(1700000600))
		if err != nil {
			t.Fatal(err)
		}
		id := result["messageId"].(string)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, id, h, 1700000600)
		if err != nil {
			t.Fatal(err)
		}
		turn := "turn-01supervisor-task-1"
		h.turns = map[string]float64{turn: 1700000000}
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000600)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{record["turnId"], answer["verified"], answer["turnOrigin"], row.State}
	})
}

func Test24_SCH_32_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheSecondReviewRoundFound.test_a_real_send_is_reported_as_one", "event", func(c *Channel, s *store.Store) []any {
		id := set2Stage(t, c, s)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(context.Background(), id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{record != nil, record["sendAttempted"] == "yes", record["deliveryState"]}
	})
}

func set2Refusal(t *testing.T, err error) Refusal {
	t.Helper()
	var r Refusal
	if !errors.As(err, &r) {
		t.Fatalf("expected refusal: %v", err)
	}
	return r
}

func Test24_SCH_36_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheThirdReviewRoundFound.test_a_readback_records_who_asserted_it_and_refuses_a_foreign_claim", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id := set2Stage(t, c, s)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		turn := record["turnId"].(string)
		_, err = c.ReadBack(ctx, id, turn, Proof(id, turn), "01someone-else", h, 1700000000)
		refusal := set2Refusal(t, err)
		var n int
		if err = s.DB.QueryRow("SELECT count(*) FROM supervisor_readbacks").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unexpected readback: %d", n)
		}
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "01supervisor-task", h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		var detail string
		if err = s.DB.QueryRow("SELECT detail FROM supervisor_readbacks WHERE message_id=?", id).Scan(&detail); err != nil {
			t.Fatal(err)
		}
		var stored map[string]any
		if err = json.Unmarshal([]byte(detail), &stored); err != nil {
			t.Fatal(err)
		}
		return []any{refusal.Reason, []any{}, answer["assertedBy"], stored["assertedBy"]}
	})
}

func Test24_SCH_42_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheSixthReviewRoundFound.test_an_uncertain_send_is_never_settled_by_the_answer_alone", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id := set2Stage(t, c, s)
		h := &set2UnknownHost{captureHost4: captureHost4{sendHost: sendHost{status: "idle"}}}
		record, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		h.items = map[string]string{}
		turn := "turn-01supervisor-task-1"
		h.turns = map[string]float64{turn: 1700000001}
		c.clockISO = func() string { return delivery.ISOOf(1700000001) }
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000001)
		if err != nil {
			t.Fatal(err)
		}
		after, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rb, err := s.SupervisorReadback(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err = s.DB.QueryRow("SELECT count(*) FROM journal WHERE kind='supervisor_message_reconciled'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unexpected reconciliation: %d", n)
		}
		return []any{record["deliveryState"], row.State, answer["verified"], answer["reconciled"], after.State, rb.Verified, []any{}}
	})
}

type set2UnknownHost struct{ captureHost4 }

func (h *set2UnknownHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends = append(h.sends, message)
	return nil, errors.New("transport outcome unknown")
}

func Test24_SCH_35_Capture(t *testing.T) {
	supervisorMirror(t, "WhatTheThirdReviewRoundFound.test_an_omission_is_refused_without_the_reading_that_found_it", "setup", func(c *Channel, s *store.Store) []any {
		reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": captureRelationID(t, c), "reason": "the turn settled without a report", "selectors": map[string]any{"state": c.StoreDirectory(), "markerRoot": "/marker", "workspace": "/tmp/workspace", "assignment": "asg-1", "session": "01child-session", "turn": "turn-unreported-1"}}
		o := ObservationObligation(reading)
		if o == nil {
			t.Fatal("missing omission")
		}
		_, err := c.Stage(context.Background(), *o, "", captureTime)
		refusal := set2Refusal(t, err)
		return []any{o.Kind, refusal.Reason, strings.Contains(refusal.Detail, "marker root")}
	})
}

func Test24_SCH_37_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFourthReviewRoundFound.test_the_rendered_readback_line_is_accepted_by_the_real_parser", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id := set2Stage(t, c, s)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		_, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		var line string
		for _, part := range strings.Split(h.sends[0], "\n") {
			if strings.Contains(part, " supervisor-read --message ") {
				line = strings.TrimSpace(part)
				break
			}
		}
		if line == "" {
			t.Fatal("missing readback command")
		}
		words := strings.Fields(line)
		if len(words) != 14 || words[0] != c.Program || words[1] != "--state" || words[2] != c.StoreDirectory() || words[3] != "--socket" || words[5] != "supervisor-read" || words[6] != "--message" || words[8] != "--turn" || words[10] != "--proof" || words[12] != "--as" {
			t.Fatalf("invalid command: %q", line)
		}
		return []any{[]any{words[0]}, words[5], words[4] != "", words[7], words[13], []any{words[4], words[9], words[11]}}
	})
}
