package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const captureTime = "2023-11-14T22:13:20.000000+00:00"

func captureEvent(t *testing.T, s *store.Store) string {
	t.Helper()
	var event string
	if err := s.DB.QueryRow("SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
		t.Fatal(err)
	}
	return event
}

func captureObligation4(t *testing.T, c *Channel, s *store.Store) Obligation {
	t.Helper()
	o, err := c.FromEvent(context.Background(), captureEvent(t, s))
	if err != nil || o == nil {
		t.Fatalf("obligation: %v, %v", o, err)
	}
	return *o
}

func captureRefusal4(t *testing.T, err error) Refusal {
	t.Helper()
	var refusal Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("expected refusal: %v", err)
	}
	return refusal
}

func captureTokens4(t *testing.T) {
	t.Helper()
	previous := TokenSource
	TokenSource = bytes.NewReader(make([]byte, 128))
	t.Cleanup(func() { TokenSource = previous })
}

type captureHost4 struct {
	sendHost
	steered bool
}

func (h *captureHost4) ReadGoalStatus(string) (any, error) { return nil, nil }
func (h *captureHost4) FindToken(thread, token string, limit int, all bool) (delivery.TokenScan, error) {
	if h.failTranscript {
		return delivery.TokenScan{}, errors.New("find_token unavailable")
	}
	return h.sendHost.FindToken(thread, token, limit, all)
}
func (h *captureHost4) ReadTurn(_ string, id string) (*delivery.TurnInfo, error) {
	if h.turns != nil {
		if at, ok := h.turns[id]; ok {
			return &delivery.TurnInfo{TurnID: id, StartedAt: &at}, nil
		}
		return nil, nil
	}
	if id != "turn-01supervisor-task-1" {
		return nil, nil
	}
	at := float64(1700000000)
	return &delivery.TurnInfo{TurnID: id, StartedAt: &at}, nil
}
func (h *captureHost4) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends = append(h.sends, message)
	h.settings = settings
	if h.items == nil {
		h.items = map[string]string{}
	}
	turn := "turn-01supervisor-task-1"
	if h.steered {
		turn = "turn-01supervisor-task-2"
	}
	h.items[turn] = message
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: turn}}, nil
}
func captureSend4(t *testing.T, c *Channel, s *store.Store, h *captureHost4) (string, map[string]any) {
	t.Helper()
	c.Settings = &delivery.TaskSettings{}
	o := captureObligation4(t, c, s)
	now := float64(1700000000)
	if h.steered {
		now = 1700000600
	}
	staged, err := c.Stage(context.Background(), o, "", delivery.ISOOf(now))
	if err != nil {
		t.Fatal(err)
	}
	id := staged["messageId"].(string)
	sent, err := c.Attempt(context.Background(), id, h, now)
	if err != nil {
		t.Fatal(err)
	}
	return id, sent
}
func Test24_SCH_16_Capture(t *testing.T) {
	for _, tc := range []struct {
		name, id, turn                 string
		hostless, empty, broken, early bool
	}{
		{"missing", "TheRoundtrip.test_a_turn_the_host_does_not_list_does_not_verify_and_is_still_recorded", "turn-nobody-has", false, false, false, false},
		{"hostless", "TheRoundtrip.test_without_a_host_nothing_about_the_turn_is_established", "turn-01supervisor-task-1", true, false, false, false},
		{"early", "TheRoundtrip.test_a_turn_that_began_before_the_send_cannot_be_the_turn_that_read_it", "turn-01supervisor-task-1", false, false, false, true},
		{"empty", "WhatTheReviewFound.test_a_real_turn_is_not_receipt_when_the_transcript_does_not_confirm_it", "turn-01supervisor-task-1", false, true, false, false},
		{"broken", "WhatTheReviewFound.test_an_unreadable_transcript_is_not_confirmation_either", "turn-01supervisor-task-1", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureTokens4(t)
			supervisorMirror(t, tc.id, "event", func(c *Channel, s *store.Store) []any {
				h := &captureHost4{sendHost: sendHost{status: "idle"}, steered: tc.early}
				if tc.early {
					c.clockISO = func() string { return delivery.ISOOf(1700000600) }
				}
				id, _ := captureSend4(t, c, s, h)
				if tc.empty {
					h.items = map[string]string{}
				}
				if tc.broken {
					h.failTranscript = true
				}
				if tc.early {
					h.turns = map[string]float64{tc.turn: 1699999400}
				}
				var adapter SendAdapter = h
				if tc.hostless {
					adapter = nil
				}
				now := float64(1700000000)
				if tc.early {
					now = 1700000600
				}
				answer, err := c.ReadBack(context.Background(), id, tc.turn, Proof(id, tc.turn), "", adapter, now)
				if err != nil {
					t.Fatal(err)
				}
				row, err := c.Get(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				delivered := answer["delivered"].(map[string]any)
				switch tc.name {
				case "missing":
					ladder, err := c.Reach(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					readback, err := s.SupervisorReadback(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					return []any{answer["verified"], row.State, ladder["received"].(map[string]any)["state"], readback.MessageID != ""}
				case "hostless":
					return []any{answer["verified"], delivered["scanned"] == true, row.State}
				case "broken":
					return []any{answer["verified"], delivered["scanned"] == true}
				case "early":
					return []any{answer["verified"], row.State}
				default:
					ladder, err := c.Reach(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					return []any{answer["verified"], delivered["found"] == true, row.State, ladder["received"].(map[string]any)["state"]}
				}
			})
		})
	}
}
func Test24_SCH_9_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "SendingIt.test_a_hierarchy_that_moved_under_a_staged_report_holds_it", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o := captureObligation4(t, c, s)
		stage, err := c.Stage(ctx, o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := stage["messageId"].(string)
		original := "bnd-24179d1961baacd1886d337c38c3eceb"
		successor := "bnd-45b86b6a2f3d253ae34532a3df0ec31f"
		if err := s.ArchiveScopeBinding(ctx, original, "archived", successor, captureTime); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertScopeBinding(ctx, store.ScopeBindingsRow{BindingID: successor, Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "01new-supervisor", HostID: "host-a", Status: "active", Revision: 2, CreatedAt: captureTime, UpdatedAt: captureTime, CWD: sql.NullString{String: "/new", Valid: true}, CXCSession: sql.NullString{String: "cxc-new", Valid: true}, HandoverNote: sql.NullString{String: "the initiative changed hands", Valid: true}, Supersedes: sql.NullString{String: original, Valid: true}}); err != nil {
			t.Fatal(err)
		}
		for _, entry := range []struct{ kind, detail string }{
			{"scope_bound", `{"role": "supervisor", "scopeKind": "initiative", "scopeKey": "INI-1", "taskId": "01new-supervisor", "revision": 2}`},
			{"scope_handover", `{"scopeKey": "INI-1", "from": "01supervisor-task", "to": "01new-supervisor", "actor": "a test", "acknowledged": []}`},
		} {
			if _, err := s.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", captureTime, entry.kind, successor, entry.detail); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.RepointScopeLink(ctx, "lnk-6fa68afd8cc27a8e7780d400a52f73c2", "active", "01parent-task", "01new-supervisor", captureTime); err != nil {
			t.Fatal(err)
		}
		_, err = c.Attempt(ctx, id, &captureHost4{sendHost: sendHost{status: "idle"}}, 1700000000)
		refusal := captureRefusal4(t, err)
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		records := make([]any, 0, len(attempts))
		for _, a := range attempts {
			records = append(records, map[string]any{"request_id": a.RequestID})
		}
		return []any{refusal.Reason, records}
	})
}
func Test24_SCH_7_Capture(t *testing.T) {
	for _, tc := range []struct {
		name, id, status string
		archived         bool
	}{
		{"busy", "SendingIt.test_a_busy_recipient_is_left_alone_with_no_attempt_to_explain", "active", false},
		{"archived", "SendingIt.test_an_archived_recipient_holds_the_report_where_it_is", "idle", true},
		{"backoff", "SendingIt.test_a_backoff_is_waited_out_rather_than_ignored", "active", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureTokens4(t)
			supervisorMirror(t, tc.id, "event", func(c *Channel, s *store.Store) []any {
				ctx := context.Background()
				c.Settings = &delivery.TaskSettings{}
				o := captureObligation4(t, c, s)
				staged, err := c.Stage(ctx, o, "", captureTime)
				if err != nil {
					t.Fatal(err)
				}
				id := staged["messageId"].(string)
				h := &captureHost4{sendHost: sendHost{status: tc.status, archived: tc.archived}}
				result, err := c.Attempt(ctx, id, h, 1700000000)
				if err != nil {
					t.Fatal(err)
				}
				row, err := c.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				attempts, err := s.SupervisorAttempts(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "backoff" {
					h.status = "idle"
					again, err := c.Attempt(ctx, id, h, 1700000000)
					if err != nil {
						t.Fatal(err)
					}
					row, err = c.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					// Python's explicit now changes eligibility, not its injected clock.
					c.clockISO = func() string { return captureTime }
					sent, err := c.Attempt(ctx, id, h, row.NextEligibleAt.Float64)
					if err != nil {
						t.Fatal(err)
					}
					return []any{again, sent != nil, row.NextEligibleAt.Valid}
				}
				requests := make([]any, 0, len(attempts))
				for _, a := range attempts {
					requests = append(requests, map[string]any{"request_id": a.RequestID})
				}
				return []any{result, row.State, requests}
			})
		})
	}
}
func Test24_SCH_8_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "SendingIt.test_the_per_recipient_bound_is_the_recipient_s_and_is_shared_on_purpose", "event", func(c *Channel, s *store.Store) []any {
		captureSend4(t, c, s, &captureHost4{sendHost: sendHost{status: "idle"}})
		var sends int
		if err := s.DB.QueryRow("SELECT sends FROM recipient_rate WHERE recipient_task_id='01supervisor-task'").Scan(&sends); err != nil {
			t.Fatal(err)
		}
		return []any{sends}
	})
}
func Test24_SCH_13_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "TheRoundtrip.test_a_delivered_message_is_read_back_and_only_then_says_received", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		id, sent := captureSend4(t, c, s, &captureHost4{sendHost: sendHost{status: "idle"}})
		ladder, err := c.Reach(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		transport := ladder["transport_accepted"].(map[string]any)["state"] == "yes"
		state := ladder["received"].(map[string]any)["state"]
		h := &captureHost4{sendHost: sendHost{status: "idle", turns: map[string]float64{sent["turnId"].(string): 1700000000}, items: map[string]string{}}}
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		h.items[sent["turnId"].(string)] = attempts[0].Message
		turn := sent["turnId"].(string)
		answer, err := c.ReadBack(ctx, id, turn, Proof(id, turn), "", h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		ladder, err = c.Reach(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		delivered := answer["delivered"].(map[string]any)
		return []any{transport, state, answer["verified"], delivered["found"], delivered["token"], strings.HasPrefix(attempts[0].DeliveryToken.String, sent["requestId"].(string)+"."), row.State, ladder["received"].(map[string]any)["state"] == "yes", ladder["received"].(map[string]any)["source"], []any{}}
	})
}
func Test24_SCH_10_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "TheSettingsGateIsNotThisChannels.test_a_send_that_cannot_say_what_it_preserves_does_not_happen", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o := captureObligation4(t, c, s)
		stage, err := c.Stage(ctx, o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := stage["messageId"].(string)
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		result, err := c.Attempt(ctx, id, h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		row, err := c.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := s.SupervisorAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := s.Journal(ctx, "supervisor_message_withheld", id)
		if err != nil {
			t.Fatal(err)
		}
		reasons := make([]any, 0, len(journal))
		for _, j := range journal {
			var detail map[string]any
			if err := json.Unmarshal([]byte(j.Detail), &detail); err != nil {
				t.Fatal(err)
			}
			reasons = append(reasons, detail)
		}
		requests := make([]any, 0, len(attempts))
		for _, a := range attempts {
			requests = append(requests, map[string]any{"request_id": a.RequestID})
		}
		return []any{result, row.State, requests, len(reasons) > 0, len(reasons) > 0 && (reasons[len(reasons)-1].(map[string]any)["reason"] == "role_policy_unconfigured" || reasons[len(reasons)-1].(map[string]any)["reason"] == "settings_unavailable")}
	})
}
func Test24_SCH_15_Capture(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		pre      bool
	}{
		{"proof", "TheRoundtrip.test_a_proof_that_is_not_this_message_and_turn_is_refused", false},
		{"unsent", "TheRoundtrip.test_nothing_is_read_back_before_it_is_delivered", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureTokens4(t)
			supervisorMirror(t, tc.id, "event", func(c *Channel, s *store.Store) []any {
				ctx := context.Background()
				var id string
				if tc.pre {
					o := captureObligation4(t, c, s)
					stage, err := c.Stage(ctx, o, "", captureTime)
					if err != nil {
						t.Fatal(err)
					}
					id = stage["messageId"].(string)
				} else {
					id, _ = captureSend4(t, c, s, &captureHost4{sendHost: sendHost{status: "idle"}})
				}
				turn := "turn-01supervisor-task-1"
				proof := "0000000000000000000000000000000000000000000000000000000000000000"
				if tc.pre {
					turn = "turn-1"
					proof = "x"
				}
				_, err := c.ReadBack(ctx, id, turn, proof, "", nil, 1700000000)
				refusal := captureRefusal4(t, err)
				if tc.pre {
					return []any{refusal.Reason}
				}
				row, err := c.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return []any{refusal.Reason, row.State}
			})
		})
	}
}
func Test24_SCH_14_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "TheRoundtrip.test_the_delivered_bytes_cannot_produce_the_proof", "event", func(c *Channel, s *store.Store) []any {
		id, sent := captureSend4(t, c, s, &captureHost4{sendHost: sendHost{status: "idle"}})
		attempts, err := s.SupervisorAttempts(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		message := attempts[0].Message
		turn := sent["turnId"].(string)
		return []any{strings.Contains(message, id), strings.Contains(message, turn), strings.Contains(message, Proof(id, turn))}
	})
}
func Test24_SCH_6_Capture(t *testing.T) {
	t.Run("frozen", func(t *testing.T) {
		captureTokens4(t)
		supervisorMirror(t, "SendingIt.test_a_send_freezes_its_bytes_and_the_bytes_carry_its_request_id", "event", func(c *Channel, s *store.Store) []any {
			h := &captureHost4{sendHost: sendHost{status: "idle"}}
			id, sent := captureSend4(t, c, s, h)
			attempts, err := s.SupervisorAttempts(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			row, err := c.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			return []any{sent["deliveryState"], sent["sendAttempted"], sent["turnId"] != nil && sent["turnId"] != "", strings.Contains(attempts[0].Message, sent["requestId"].(string)), row.State}
		})
	})
}
func Test24_SCH_5_Capture(t *testing.T) {
	t.Run("decision", func(t *testing.T) {
		supervisorMirror(t, "WhatTheMessageCarries.test_a_decision_upward_names_what_is_being_decided", "event", func(c *Channel, s *store.Store) []any {
			ctx := context.Background()
			o := captureObligation4(t, c, s)
			kind := o.Kind
			r, err := c.Resolve(ctx, o.RelationID)
			if err != nil {
				t.Fatal(err)
			}
			p, err := c.Compose(ctx, o, r, captureTime)
			if err != nil {
				t.Fatal(err)
			}
			region := p["envelope"].(map[string]any)
			return []any{kind, region["kind"], region["answerOwedBy"], strings.TrimSpace(region["decision"].(string)) != ""}
		})
	})
}
func Test24_SCH_4_Capture(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		supervisorMirror(t, "WhatMayBeStaged.test_an_ordinary_event_is_not_news_and_has_no_obligation_to_stage", "ordinary-final", func(c *Channel, s *store.Store) []any {
			o, err := c.FromEvent(context.Background(), captureEvent(t, s))
			if err != nil {
				t.Fatal(err)
			}
			return []any{o}
		})
	})
	t.Run("reported", func(t *testing.T) {
		supervisorMirror(t, "WhatMayBeStaged.test_a_fact_already_reported_is_refused_and_the_obligation_stays_standing", "event", func(c *Channel, s *store.Store) []any {
			ctx := context.Background()
			o := captureObligation4(t, c, s)
			message := "deadbeef"
			_, err := c.RecordReport(ctx, o, captureTime, &message, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Stage(ctx, o, "", captureTime)
			refusal := captureRefusal4(t, err)
			rows, err := s.DB.QueryContext(ctx, "SELECT message_id FROM supervisor_messages")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			messages := []any{}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				messages = append(messages, map[string]any{"message_id": id})
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return []any{refusal.Reason, strings.Contains(refusal.Detail, "already_reported_under_this_obligation"), "standing", messages}
		})
	})
	t.Run("recipient", func(t *testing.T) {
		supervisorMirror(t, "WhatMayBeStaged.test_a_caller_naming_another_recipient_is_the_finding", "event", func(c *Channel, s *store.Store) []any {
			_, err := c.Stage(context.Background(), captureObligation4(t, c, s), "01someone-else", captureTime)
			refusal := captureRefusal4(t, err)
			return []any{refusal.Reason, strings.Contains(refusal.Detail, "01supervisor-task")}
		})
	})
}
