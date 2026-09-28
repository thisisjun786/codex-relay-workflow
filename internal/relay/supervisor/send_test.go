package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

type sendHost struct {
	status         string
	archived       bool
	sends          []string
	settings       *delivery.TaskSettings
	turns          map[string]float64
	items          map[string]string
	failTranscript bool
	beforeSend     func()
	outcome        string
}

func (h *sendHost) ReadThread(string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: h.status, CanAcceptInput: &yes}, nil
}
func (h *sendHost) IsArchived(string, any) (*bool, error) { return &h.archived, nil }
func (h *sendHost) ReadGoalStatus(string) (any, error)    { return "", nil }
func (h *sendHost) ListTurnIDs(string, int) ([]any, error) {
	return []any{"turn-supervisor-1"}, nil
}
func (h *sendHost) ReadTurn(_ string, id string) (*delivery.TurnInfo, error) {
	if h.turns != nil {
		if at, ok := h.turns[id]; ok {
			return &delivery.TurnInfo{TurnID: id, StartedAt: &at}, nil
		}
		return nil, nil
	}
	at := float64(1_700_000_001)
	if id != "turn-supervisor-1" {
		return nil, nil
	}
	return &delivery.TurnInfo{TurnID: id, StartedAt: &at}, nil
}
func (h *sendHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	if h.beforeSend != nil {
		h.beforeSend()
	}
	h.sends = append(h.sends, message)
	h.settings = settings
	if h.items == nil {
		h.items = map[string]string{}
	}
	h.items["turn-supervisor-1"] = message
	if h.outcome == "unknown" {
		return nil, errors.New("transport outcome unknown")
	}
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: "turn-supervisor-1"}}, nil
}
func (h *sendHost) GetOperation(string) (delivery.Obj, error) { return nil, nil }
func (h *sendHost) FindToken(_ string, token string, _ int, _ bool) (delivery.TokenScan, error) {
	if h.failTranscript {
		return delivery.TokenScan{}, errors.New("transcript unavailable")
	}
	for turn, text := range h.items {
		if strings.Contains(text, token) {
			return delivery.TokenScan{Found: true, TurnID: turn, Exhausted: true, Scanned: 1}, nil
		}
	}
	return delivery.TokenScan{Found: false, Exhausted: true, Scanned: len(h.items)}, nil
}
func (h *sendHost) FindDispatchedTurn(string, string, float64) (delivery.TurnPresence, error) {
	return delivery.TurnPresence{}, nil
}
func (h *sendHost) FindTokenSince(string, string, []string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{}, nil
}
func (h *sendHost) FindTokenInTurn(string, string, string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{}, nil
}
func (h *sendHost) RecipientFingerprint(string) (string, error) { return "", nil }

func Test24_SCH_6_FrozenSendAndSettings(t *testing.T) {
	f := fixture24(t)
	f.c.Socket = "/tmp/host.sock"
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	host := &sendHost{status: "idle"}
	result, err := f.c.Attempt(f.ctx, id, host, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if result["deliveryState"] != "dispatched" || result["sendAttempted"] != "yes" || result["turnId"] != "turn-supervisor-1" {
		t.Fatalf("send %v", result)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts %v %v", attempts, err)
	}
	if len(host.sends) != 1 || host.sends[0] != attempts[0].Message || !strings.Contains(attempts[0].Message, result["requestId"].(string)) || host.settings != f.c.Settings {
		t.Fatalf("host/frozen mismatch: %+v %v", attempts, host.sends)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "dispatched" {
		t.Fatalf("message %+v %v", row, err)
	}
}
func Test24_SCH_7_BusyArchivedAndBackoff(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		archived     bool
		state        string
		delay        float64
	}{{"busy", "active", false, "deferred_busy", 15}, {"archived", "idle", true, "withheld_pre_send", 60}} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture24(t)
			f.c.Settings = &delivery.TaskSettings{}
			_, staged := f.staged(t)
			id := staged["messageId"].(string)
			host := &sendHost{status: tc.status, archived: tc.archived}
			result, err := f.c.Attempt(f.ctx, id, host, 1_700_000_000)
			if err != nil || result != nil {
				t.Fatalf("attempt %v %v", result, err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil || row.State != tc.state || !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != 1_700_000_000+tc.delay {
				t.Fatalf("row %+v %v", row, err)
			}
			attempts, err := f.s.SupervisorAttempts(f.ctx, id)
			if err != nil || len(attempts) != 0 || len(host.sends) != 0 {
				t.Fatalf("attempts %v sends %v err %v", attempts, host.sends, err)
			}
			result, err = f.c.Attempt(f.ctx, id, host, row.NextEligibleAt.Float64-1)
			if err != nil || result != nil {
				t.Fatalf("ignored backoff: %v %v", result, err)
			}
		})
	}
}
func Test24_SCH_8_SharedRecipientRate(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	host := &sendHost{status: "idle"}
	if _, err := f.c.Attempt(f.ctx, staged["messageId"].(string), host, 1_700_000_000); err != nil {
		t.Fatal(err)
	}
	var sends int64
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT sends FROM recipient_rate WHERE recipient_task_id='supervisor'").Scan(&sends); err != nil || sends != 1 {
		t.Fatalf("sends %d err %v", sends, err)
	}
}
func Test24_SCH_9_StagedHandoverHasNoAttempt(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	if err := f.s.ArchiveScopeBinding(f.ctx, "b-supervisor", "archived", "b-next", f.at); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO scope_bindings(binding_id,role,scope_kind,scope_key,task_id,host_id,status,revision,created_at,updated_at) VALUES ('b-next','supervisor','initiative','INI-1','next','host','active',2,'t','t')")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Attempt(f.ctx, staged["messageId"].(string), &sendHost{status: "idle"}, 1_700_000_000)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "relation_owner_drift" {
		t.Fatalf("handover %v", err)
	}
	var count int
	if err = f.s.DB.QueryRowContext(context.Background(), "SELECT count(*) FROM supervisor_attempts").Scan(&count); err != nil || count != 0 {
		t.Fatalf("attempts %d %v", count, err)
	}
}
func Test24_SCH_10_RealSettingsGate(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	host := &sendHost{status: "idle"}
	result, err := f.c.Attempt(f.ctx, id, host, 1_700_000_000)
	if err != nil || result != nil {
		t.Fatalf("attempt %v %v", result, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "withheld_pre_send" {
		t.Fatalf("row %v %v", row, err)
	}
	var count int
	if err = f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_attempts").Scan(&count); err != nil || count != 0 || len(host.sends) != 0 {
		t.Fatalf("attempts %d sends %d err %v", count, len(host.sends), err)
	}
	journal, err := f.s.Journal(f.ctx, "supervisor_message_withheld", id)
	if err != nil || len(journal) != 1 || !strings.Contains(journal[0].Detail, "settings_unavailable") {
		t.Fatalf("journal %+v %v", journal, err)
	}
}
