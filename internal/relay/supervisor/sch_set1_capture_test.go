package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func set1Delivered(t *testing.T, c *Channel, s *store.Store) (string, map[string]any, *captureHost4) {
	t.Helper()
	h := &captureHost4{sendHost: sendHost{status: "idle"}}
	id, sent := captureSend4(t, c, s, h)
	return id, sent, h
}
func set1Read(t *testing.T, c *Channel, id, turn string, h *captureHost4) map[string]any {
	t.Helper()
	r, err := c.ReadBack(context.Background(), id, turn, Proof(id, turn), "", h, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func capturePriorMessage(t *testing.T, s *store.Store) string {
	t.Helper()
	var id string
	if err := s.DB.QueryRow("SELECT message_id FROM supervisor_readbacks LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type set1Host17 struct{ captureHost4 }

func (h *set1Host17) FindToken(_ context.Context, _ string, token string, _ int, _ bool) (delivery.TokenScan, error) {
	for turn, text := range h.items {
		if strings.Contains(text, token) {
			return delivery.TokenScan{Found: true, TurnID: turn, Scanned: 1, Exhausted: turn != "turn-01supervisor-task-2"}, nil
		}
	}
	return delivery.TokenScan{Exhausted: true, Scanned: len(h.items)}, nil
}
func Test24_SCH_17_Capture(t *testing.T) {
	captureTokens21(t)
	TokenSource.(*captureTokenReader21).next = 1
	supervisorMirror(t, "TheRoundtrip.test_which_turn_answered_is_recorded_rather_than_averaged_into_one_word", "event", func(c *Channel, s *store.Store) []any {
		// The event snapshot is taken after the first readback and before the second stage.
		prior, err := s.SupervisorReadback(context.Background(), capturePriorMessage(t, s))
		if err != nil {
			t.Fatal(err)
		}
		origin := readDetail(prior)["turnOrigin"]
		o := captureObligation4(t, c, s)
		stage, err := c.Stage(context.Background(), o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := stage["messageId"].(string)
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}, steered: true}
		c.clockISO = func() string { return captureTime }
		// Python's clock remains at the fixture time; the explicit now only admits the queued row.
		_, err = c.Attempt(context.Background(), id, h, 1700003600)
		if err != nil {
			t.Fatal(err)
		}
		turn := "turn-01supervisor-task-3"
		h.turns = map[string]float64{turn: 1700003600, "turn-01supervisor-task-2": 1700000000}
		if len(h.sends) == 0 {
			t.Fatalf("second send withheld: %+v", h)
		}
		answer, err := c.ReadBack(context.Background(), id, turn, Proof(id, turn), "", &set1Host17{*h}, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{origin, turn, answer["verified"], answer["turnOrigin"]}
	})
}
func Test24_SCH_17_RecipientTurnCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheFourthReviewRoundFound.test_a_turn_the_recipient_opened_is_not_required_to_hold_the_delivered_bytes", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		_ = sent
		turn := "turn-01supervisor-task-2"
		h.turns = map[string]float64{turn: 1700000030, "turn-01supervisor-task-1": 1700000000}
		c.clockISO = func() string { return delivery.ISOOf(1700000030) }
		answer, err := c.ReadBack(context.Background(), id, turn, Proof(id, turn), "", h, 1700000030)
		if err != nil {
			t.Fatal(err)
		}
		return []any{answer["turnOrigin"], answer["verified"]}
	})
}
func Test24_SCH_11_Capture(t *testing.T) {
	supervisorMirror(t, "TheTwoQueuesAreDisjoint.test_the_supervisor_queue_holds_no_deliveries_and_the_delivery_queue_no_messages", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		event := captureEvent(t, s)
		// Mirror the Python delivery enqueue before staging the supervisor report.
		d := delivery.NewService(s, &delivery.FakeClock{T: 1700000000})
		if _, err := d.Enqueue(ctx, event, delivery.Completion, ""); err != nil {
			t.Fatal(err)
		}
		o := captureObligation4(t, c, s)
		staged, err := c.Stage(ctx, o, "", captureTime)
		if err != nil {
			t.Fatal(err)
		}
		id := staged["messageId"].(string)
		_, err = c.Get(ctx, event)
		first := captureRefusal4(t, err)
		_, err = d.Get(ctx, id)
		if err == nil || !strings.Contains(err.Error(), "not_claimable") {
			t.Fatalf("delivery refusal: %v", err)
		}
		return []any{id, first.Reason, "not_claimable", []any{event}}
	})
}
func Test24_SCH_12_Capture(t *testing.T) {
	supervisorMirror(t, "TheTwoQueuesAreDisjoint.test_the_oldest_staged_message_is_sent_first", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o := captureObligation4(t, c, s)
		if _, err := c.Stage(ctx, o, "", delivery.ISOOf(1700000005)); err != nil {
			t.Fatal(err)
		}
		rows, err := storeseed.EligibleSupervisorMessages(ctx, c.Store, 1700000005, 100)
		if err != nil {
			t.Fatal(err)
		}
		ids := []any{}
		for _, row := range rows {
			ids = append(ids, row.MessageID)
		}
		return []any{ids}
	})
}
func Test24_SCH_18_ConvergesCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "TheRoundtrip.test_a_second_readback_of_one_message_converges_on_the_first", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		turn := sent["turnId"].(string)
		first := set1Read(t, c, id, turn, h)
		again := set1Read(t, c, id, turn, h)
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_readbacks").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return []any{first["recorded"], again["recorded"], again["verified"], n}
	})
}
func Test24_SCH_18_InvalidProofCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "TheReadbackRaceIsClosedInTheWrite.test_a_settled_message_answers_without_checking_the_proof_it_was_handed", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		set1Read(t, c, id, sent["turnId"].(string), h)
		answer, err := c.ReadBack(context.Background(), id, "turn-anything", "not-a-proof", "", h, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		return []any{answer["recorded"], answer["verified"], answer["readTurnId"]}
	})
}
func Test24_SCH_18_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "WhatTheReviewFound.test_an_unverified_readback_cannot_replace_a_verified_one", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		first := set1Read(t, c, id, sent["turnId"].(string), h)
		second := set1Read(t, c, id, "turn-nobody-has", h)
		row, err := s.SupervisorReadback(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		ladder, err := c.Reach(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{first["verified"], second["recorded"], second["verified"], row.Verified, ladder["received"].(map[string]any)["state"] == "yes"}
	})
}
func Test24_SCH_19_LadderCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "TheRoundtrip.test_a_readback_never_credits_the_supervisor_with_agreeing", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		set1Read(t, c, id, sent["turnId"].(string), h)
		reach, err := c.Reach(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{reach["agreed"].(map[string]any)["state"], reach["applied"].(map[string]any)["state"], reach["verified"].(map[string]any)["state"]}
	})
}
func Test24_SCH_19_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "TheRoundtrip.test_a_verified_readback_does_not_discharge_the_reporting_obligation", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		set1Read(t, c, id, sent["turnId"].(string), h)
		o := captureObligation4(t, c, s)
		can, _, err := c.Reportable(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if can {
			return []any{"not_standing"}
		}
		return []any{"standing"}
	})
}
func Test24_SCH_20_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		set1Read(t, c, id, sent["turnId"].(string), h)
		shown, err := c.Show(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{shown["state"], shown["recipient"], len(shown["attempts"].([]any)), shown["readback"].(map[string]any)["verified"], shown["packet"].(map[string]any)["version"], strings.Contains(shown["limits"].(string), "Linear record")}
	})
}
func Test24_SCH_22_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheReviewFound.test_naming_a_newer_message_does_not_send_it_ahead_of_an_older_one", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		first := capturePriorQueued(t, s)
		o := captureObligation4(t, c, s)
		second, err := c.Stage(ctx, o, "", delivery.ISOOf(1700000005))
		if err != nil {
			t.Fatal(err)
		}
		c.Settings = &delivery.TaskSettings{}
		h := &captureHost4{sendHost: sendHost{status: "idle"}}
		_, err = c.Attempt(ctx, second["messageId"].(string), h, 1700000005)
		refusal := captureRefusal4(t, err)
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_attempts").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unexpected attempts: %d", n)
		}
		TokenSource.(*captureTokenReader21).next = 0
		c.clockISO = func() string { return delivery.ISOOf(1700000005) }
		a, err := c.Attempt(ctx, first, h, 1700000005)
		if err != nil {
			t.Fatal(err)
		}
		c.clockISO = func() string { return delivery.ISOOf(1700003605) }
		h.steered = true
		b, err := c.Attempt(ctx, second["messageId"].(string), h, 1700003605)
		if err != nil {
			t.Fatal(err)
		}
		return []any{refusal.Reason, strings.Contains(refusal.Detail, first), []any{}, a != nil, b != nil}
	})
}
func capturePriorQueued(t *testing.T, s *store.Store) string {
	t.Helper()
	var id string
	if err := s.DB.QueryRow("SELECT message_id FROM supervisor_messages ORDER BY rowid LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func set1ClaimWithoutTransport(t *testing.T, c *Channel, s *store.Store, id, owner string, now float64) {
	t.Helper()
	ctx := context.Background()
	row, err := c.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	packet := Packet(evidence.Dict(evidence.Decode(row.Packet), false))
	request := "sup-" + id[:12] + "-a1"
	message := c.render(packet, request, request+".0000000000000000")
	at := delivery.ISOOf(now)
	if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state='sending',attempt_count=1,lease_owner=?,lease_until=?,updated_at=? WHERE message_id=?", owner, now+300, at, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_attempts(request_id,message_id,attempt_no,message,state,send_attempted,retry_safe,record,sent_at,observed_at,delivery_token) VALUES(?,?,1,?,'held_uncertain','unknown',0,?,?,?,?)", request, id, message, pyjson.Dumps(map[string]any{"requestId": request, "messageId": id, "attemptNo": 1, "deliveryState": "held_uncertain"}, pyjson.Options{SortKeys: true, Unicode: true}), at, at, request+".0000000000000000"); err != nil {
		t.Fatal(err)
	}
}
func Test24_SCH_23_Capture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheReviewFound.test_a_held_older_message_does_not_block_the_queue_behind_it", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		o := captureObligation4(t, c, s)
		second, err := c.Stage(ctx, o, "", delivery.ISOOf(1700000005))
		if err != nil {
			t.Fatal(err)
		}
		c.Settings = &delivery.TaskSettings{}
		result, err := c.Attempt(ctx, second["messageId"].(string), &captureHost4{sendHost: sendHost{status: "idle"}}, 1700000005)
		if err != nil {
			t.Fatal(err)
		}
		return []any{result != nil}
	})
}

func Test24_SCH_22_InFlightCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheMergeBoundaryReviewFound.test_an_older_send_in_flight_holds_the_one_behind_it", "event", func(c *Channel, s *store.Store) []any {
		first := capturePriorQueued(t, s)
		o := captureObligation4(t, c, s)
		second, err := c.Stage(context.Background(), o, "", delivery.ISOOf(1700000005))
		if err != nil {
			t.Fatal(err)
		}
		set1ClaimWithoutTransport(t, c, s, first, "in flight", 1700000005)
		c.Settings = &delivery.TaskSettings{}
		row, err := c.Get(context.Background(), first)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Attempt(context.Background(), second["messageId"].(string), &captureHost4{sendHost: sendHost{status: "idle"}}, 1700000005)
		refusal := captureRefusal4(t, err)
		return []any{row.State, refusal.Reason, strings.Contains(refusal.Detail, first)}
	})
}
func Test24_SCH_23_StrandedCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "WhatTheMergeBoundaryReviewFound.test_a_stranded_older_send_does_not_hold_it_for_ever", "event", func(c *Channel, s *store.Store) []any {
		first := capturePriorQueued(t, s)
		o := captureObligation4(t, c, s)
		second, err := c.Stage(context.Background(), o, "", delivery.ISOOf(1700000005))
		if err != nil {
			t.Fatal(err)
		}
		set1ClaimWithoutTransport(t, c, s, first, "a worker that died", 1700000005)
		TokenSource.(*captureTokenReader21).next = 1
		c.Settings = &delivery.TaskSettings{}
		c.clockISO = func() string { return delivery.ISOOf(1700000005) }
		answer, err := c.Attempt(context.Background(), second["messageId"].(string), &captureHost4{sendHost: sendHost{status: "idle"}}, 1700000306)
		if err != nil {
			t.Fatal(err)
		}
		return []any{answer != nil}
	})
}

type set1RaceHost struct {
	captureHost4
	beforeScan func()
}

func (h *set1RaceHost) FindToken(_ context.Context, thread, token string, limit int, all bool) (delivery.TokenScan, error) {
	h.beforeScan()
	return h.captureHost4.FindToken(context.Background(), thread, token, limit, all)
}
func Test24_SCH_27_RacedCapture(t *testing.T) {
	captureTokens21(t)
	supervisorMirror(t, "TheReadbackRaceIsClosedInTheWrite.test_a_verified_readback_survives_a_racing_unverified_one", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		var first map[string]any
		race := &set1RaceHost{captureHost4: *h}
		race.beforeScan = func() { first = set1Read(t, c, id, sent["turnId"].(string), h) }
		second, err := c.ReadBack(context.Background(), id, "turn-nobody-has", Proof(id, "turn-nobody-has"), "", race, 1700000000)
		if err != nil {
			t.Fatal(err)
		}
		row, err := s.SupervisorReadback(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		message, err := c.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		reach, err := c.Reach(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return []any{first["verified"], second["recorded"], second["raced"] == true, second["verified"], row.Verified, row.ReadTurnID, message.State, reach["received"].(map[string]any)["state"] == "yes"}
	})
}
func Test24_SCH_27_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "TheReadbackRaceIsClosedInTheWrite.test_an_unsettled_row_is_still_updated_by_the_next_answer", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		first := set1Read(t, c, id, "turn-nobody-has", h)
		second := set1Read(t, c, id, sent["turnId"].(string), h)
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_readbacks").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return []any{first["verified"], second["recorded"], second["verified"], n}
	})
}
func set1Command(t *testing.T, binary string, args ...string) (int, map[string]any, string) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"relay"}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("command: %v %s", err, out)
		}
	}
	var answer map[string]any
	_ = json.Unmarshal(out, &answer)
	return code, answer, string(out)
}
func Test24_SCH_24_NoHostCapture(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		send     bool
	}{
		{"send", "TheHostRequiredCommandsRefuseWithoutOne.test_a_send_with_no_host_refuses_instead_of_recording_a_withholding", true},
		{"read", "TheHostRequiredCommandsRefuseWithoutOne.test_a_readback_with_no_host_refuses_instead_of_recording_an_unverified_one", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := testsupport.CRW(t)
			captureTokens21(t)
			supervisorMirror(t, tc.id, "event", func(c *Channel, s *store.Store) []any {
				ctx := context.Background()
				var id, turn string
				if tc.send {
					o := captureObligation4(t, c, s)
					staged, err := c.Stage(ctx, o, "", captureTime)
					if err != nil {
						t.Fatal(err)
					}
					id = staged["messageId"].(string)
				} else {
					var record map[string]any
					id, record, _ = set1Delivered(t, c, s)
					turn = record["turnId"].(string)
				}
				args := []string{"--state", c.StoreDirectory(), "supervisor-send", "--message", id}
				if !tc.send {
					args = []string{"--state", c.StoreDirectory(), "supervisor-read", "--message", id, "--turn", turn, "--proof", Proof(id, turn), "--as", "01supervisor-task"}
				}
				code, answer, text := set1Command(t, binary, args...)
				if code != 4 || answer["error"] != "usage" {
					t.Fatalf("missing host: code %d answer %v output %q", code, answer, text)
				}
				asked := strings.Contains(answer["detail"].(string), "--socket")
				if tc.send {
					var n int
					if err := s.DB.QueryRow("SELECT count(*) FROM recipient_lifecycle").Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Fatalf("unexpected lifecycle rows: %d", n)
					}
					row, err := c.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					return []any{asked, []any{}, row.State}
				}
				var n int
				if err := s.DB.QueryRow("SELECT count(*) FROM supervisor_readbacks").Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("unexpected readbacks: %d", n)
				}
				return []any{asked, []any{}}
			})
		})
	}
}
func Test24_SCH_25_CommandClassificationCapture(t *testing.T) {
	binary := testsupport.CRW(t)
	supervisorMirror(t, "TheHostRequiredCommandsRefuseWithoutOne.test_both_are_declared_host_required_as_well_as_enforced", "setup", func(c *Channel, s *store.Store) []any {
		code, answer, text := set1Command(t, binary, "--state", t.TempDir(), "doctor")
		if code != 0 {
			t.Fatalf("doctor: %d %v %s", code, answer, text)
		}
		raw := answer["actorReachability"].(map[string]any)
		contains := func(key, value string) bool {
			for _, item := range raw[key].([]any) {
				if item == value {
					return true
				}
			}
			return false
		}
		return []any{contains("hostRequiredCommands", "supervisor-send"), contains("hostRequiredCommands", "supervisor-read"), contains("offlineCommands", "supervisor-stage"), contains("offlineCommands", "supervisor-show")}
	})
}
func Test24_SCH_26_StageShapeCapture(t *testing.T) {
	binary := testsupport.CRW(t)
	for _, tc := range []struct {
		name, id string
		replay   func(*testing.T, string) []any
	}{
		{"refused", "TheHostRequiredCommandsRefuseWithoutOne.test_a_subject_that_cannot_be_used_is_refused_rather_than_ignored", func(t *testing.T, b string) []any {
			first, one, _ := set1Command(t, b, "supervisor-stage", "--event", "abc", "--observation", "a.json")
			second, two, _ := set1Command(t, b, "supervisor-stage", "--project", "PRJ-1", "--recipient", "01someone")
			if first != 4 || second != 4 {
				t.Fatalf("stage refusals: %d %v; %d %v", first, one, second, two)
			}
			return []any{strings.Contains(one["detail"].(string), "--observation"), strings.Contains(two["detail"].(string), "--recipient")}
		}},
		{"observation", "TheHostRequiredCommandsRefuseWithoutOne.test_a_standalone_observation_is_a_subject_of_its_own", func(t *testing.T, b string) []any {
			_, _, text := set1Command(t, b, "supervisor-stage", "--observation", "a.json")
			if !strings.Contains(text, "a.json") {
				t.Fatalf("observation argument ignored: %s", text)
			}
			return []any{[]any{"a.json"}, nil, nil}
		}},
		{"socket", "TheHostRequiredCommandsRefuseWithoutOne.test_the_socket_is_global_and_belongs_before_the_subcommand", func(t *testing.T, b string) []any {
			// Parsing is the property: before the subcommand, --socket parses and the command runs
			// (here it refuses: nothing is staged); after it, argparse rejects the call with usage.
			// Both exit 2, so the exit code alone cannot tell them apart.
			_, parsed, _ := set1Command(t, b, "--socket", "/tmp/s", "supervisor-send", "--message", "m")
			after, _, text := set1Command(t, b, "supervisor-send", "--message", "m", "--socket", "/tmp/s")
			if parsed["error"] != "refused" || after != 2 || !strings.Contains(text, "usage:") {
				t.Fatalf("socket placement: before %v; after %d %q", parsed, after, text)
			}
			return []any{"/tmp/s"}
		}},
		{"project", "TheHostRequiredCommandsRefuseWithoutOne.test_project_staging_accepts_the_readings_only_it_can_see", func(t *testing.T, b string) []any {
			first := filepath.Join(t.TempDir(), "first.json")
			if err := os.WriteFile(first, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, text := set1Command(t, b, "supervisor-stage", "--project", "PRJ-1", "--observation", first, "--observation", "b.json")
			if !strings.Contains(text, "b.json") {
				t.Fatalf("second observation ignored: %s", text)
			}
			return []any{"PRJ-1", []any{"a.json", "b.json"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supervisorMirror(t, tc.id, "setup", func(_ *Channel, _ *store.Store) []any { return tc.replay(t, binary) })
		})
	}
}
func Test24_SCH_28_Capture(t *testing.T) {
	captureTokens4(t)
	supervisorMirror(t, "WhichTurnAnsweredCanBeUnknown.test_a_settled_uncertain_attempt_with_no_turn_id_leaves_the_origin_unknown", "event", func(c *Channel, s *store.Store) []any {
		id, sent, h := set1Delivered(t, c, s)
		ctx := context.Background()
		if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_attempts SET state='held_uncertain',turn_id=NULL WHERE message_id=?", id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state='held_uncertain' WHERE message_id=?", id); err != nil {
			t.Fatal(err)
		}
		answer := set1Read(t, c, id, sent["turnId"].(string), h)
		return []any{answer["turnOrigin"]}
	})
}
