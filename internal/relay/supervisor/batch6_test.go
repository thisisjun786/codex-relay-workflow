package supervisor

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test24_SCH_59_AlteredObligationNamesField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Obligation)
	}{
		{"executionGeneration", func(o *Obligation) { n := int64(99); o.Generation = &n }},
		{"issueKey", func(o *Obligation) { issue := "OTHER-1"; o.Issue = &issue }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture24(t)
			o := f.obligation(t)
			tc.mutate(&o)
			_, err := f.c.Stage(f.ctx, o, "", f.at)
			var refusal Refusal
			if !errors.As(err, &refusal) || refusal.Reason != "contradictory_observation" || !strings.Contains(refusal.Detail, tc.name+" differ") {
				t.Fatalf("refusal %v", err)
			}
			var count int
			if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_messages").Scan(&count); err != nil || count != 0 {
				t.Fatalf("messages %d %v", count, err)
			}
			if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM journal WHERE kind='supervisor_report'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("reports %d %v", count, err)
			}
		})
	}
}

func Test24_SCH_58_RecoveryBeforeLateSuccessKeepsUncertain(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	h := &sendHost{status: "idle"}
	h.beforeSend = func() {
		if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET lease_until=? WHERE message_id=?", float64(1_700_000_000), id); err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		kept, err := f.c.recoverStranded(f.ctx, row, 1_700_000_001)
		if err != nil || kept.State != "held_uncertain" {
			t.Fatalf("recovery %+v %v", kept, err)
		}
	}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer["deliveryState"] != "dispatched" || answer["messageState"] != "held_uncertain" {
		t.Fatalf("late %v %v", answer, err)
	}
	journal, err := f.s.Journal(f.ctx, "supervisor_message_attempted", id)
	if err != nil || len(journal) != 1 {
		t.Fatalf("journal %+v %v", journal, err)
	}
	var recorded map[string]any
	if err := json.Unmarshal([]byte(journal[0].Detail), &recorded); err != nil || recorded["messageMoved"] != false {
		t.Fatalf("journal %+v %v", journal, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "held_uncertain" {
		t.Fatalf("row %+v %v", row, err)
	}
}

func Test24_SCH_65_RecoveredClaimCannotStartTransport(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	f.c.beforeTransport = func() {
		if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET lease_until=? WHERE message_id=?", float64(1_700_000_000), id); err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := f.c.recoverStranded(f.ctx, row, 1_700_000_001)
		if err != nil || recovered.State != "queued" {
			t.Fatalf("recovery %+v %v", recovered, err)
		}
	}
	h := &sendHost{status: "idle"}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer != nil || len(h.sends) != 0 {
		t.Fatalf("lapsed %v %v %v", answer, err, h.sends)
	}
	f.c.beforeTransport = nil
	answer, err = f.c.Attempt(f.ctx, id, h, 1_700_000_002)
	if err != nil || answer["deliveryState"] != "dispatched" || len(h.sends) != 1 {
		t.Fatalf("retry %v %v %v", answer, err, h.sends)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 2 || attempts[0].SendAttempted != "no" || attempts[1].SendAttempted != "yes" {
		t.Fatalf("attempts %+v %v", attempts, err)
	}
}

func Test24_SCH_67_TransportStartPacingDefers(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	f.c.beforeTransport = func() {
		service := delivery.NewService(f.s, delivery.SystemClock{})
		if refused, err := service.ReserveSend(f.ctx, "supervisor", 1_700_000_000); err != nil || refused != "" {
			t.Fatalf("other sender %q %v", refused, err)
		}
	}
	h := &sendHost{status: "idle"}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer != nil || len(h.sends) != 0 {
		t.Fatalf("paced %v %v %v", answer, err, h.sends)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "queued" || row.HoldReason.Valid || !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != 1_700_000_005 {
		t.Fatalf("deferred %+v %v", row, err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || attempts[0].SendAttempted != "no" || attempts[0].TransportStartedAt.Valid {
		t.Fatalf("attempts %+v %v", attempts, err)
	}
	f.c.beforeTransport = nil
	answer, err = f.c.Attempt(f.ctx, id, h, row.NextEligibleAt.Float64)
	if err != nil || answer["deliveryState"] != "dispatched" || len(h.sends) != 1 {
		t.Fatalf("retry %v %v %v", answer, err, h.sends)
	}
}

func Test24_SCH_52_InvalidTurnStartCannotVerify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start *float64
	}{
		{"missing", nil},
		{"nan", func() *float64 { n := math.NaN(); return &n }()},
		{"infinite", func() *float64 { n := math.Inf(1); return &n }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h, id, _ := delivered24(t)
			host := &turnStartHost{sendHost: h, started: tc.start}
			answer, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", host, 1_700_000_002)
			if err != nil || answer["verified"] != "unverified_turn" {
				t.Fatalf("readback %v %v", answer, err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil || row.State != "dispatched" {
				t.Fatalf("message %+v %v", row, err)
			}
		})
	}
}

// The readback reads the named turn's start by supervisorchannel._host_time, the rule every
// host-time reader shares: a string float() reads is the time it spells, anything else that is
// not a finite number is no start and does not verify.
func Test24_SCH_52b_TurnStartIsReadAsAHostTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started any
		want    string
	}{
		{"numeric-string", "1700000001", "host_read"},
		{"spaced-underscored", " 1_700_000_001 ", "host_read"},
		{"numeric-string-before-send", "1000", "turn_predates_send"},
		{"text", "bad", "unverified_turn"},
		{"bool", true, "unverified_turn"},
		{"nan-text", "nan", "unverified_turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h, id, _ := delivered24(t)
			host := &turnStartHost{sendHost: h, started: tc.started}
			answer, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", host, 1_700_000_002)
			if err != nil || answer["verified"] != tc.want {
				t.Fatalf("readback %v %v", answer, err)
			}
		})
	}
}

// turnStartHost answers every turn it is asked for with started as its start, whatever that is: a
// *float64 (nil, NaN or infinite included) or any other value a host might give.
type turnStartHost struct {
	*sendHost
	started any
}

func (h *turnStartHost) ReadTurn(_ string, id string) (*delivery.TurnInfo, error) {
	return &delivery.TurnInfo{TurnID: id, StartedAt: h.started}, nil
}

func Test24_SCH_51_HandoverAtTransportStartCancelsClaim(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	o, stage := f.staged(t)
	id := stage["messageId"].(string)
	f.c.beforeTransport = func() {
		if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "b-successor", f.at); err != nil {
			t.Fatal(err)
		}
		if err := storeseed.InsertScopeBinding(f.ctx, f.s, store.ScopeBindingsRow{BindingID: "b-successor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "successor", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t2", UpdatedAt: "t2"}); err != nil {
			t.Fatal(err)
		}
		if err := storeseed.RepointScopeLink(f.ctx, f.s, "lnk-project", "active", "parent", "successor", f.at); err != nil {
			t.Fatal(err)
		}
	}
	h := &sendHost{status: "idle"}
	_, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "relation_owner_drift" || !strings.Contains(refusal.Detail, "Nothing was sent") || len(h.sends) != 0 {
		t.Fatalf("send %v host %v", err, h.sends)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || attempts[0].SendAttempted != "no" || attempts[0].RetrySafe != 1 || attempts[0].TransportStartedAt.Valid {
		t.Fatalf("attempts %+v %v", attempts, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "queued" {
		t.Fatalf("row %+v %v", row, err)
	}
	moved, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil || moved["readdressed"] != true {
		t.Fatalf("restage %v %v", moved, err)
	}
	f.c.beforeTransport = nil
	sent, err := f.c.Attempt(f.ctx, id, h, 1_700_000_001)
	if err != nil || sent["recipientTaskId"] != "successor" || len(h.sends) != 1 {
		t.Fatalf("successor %v %v %v", sent, err, h.sends)
	}
}
