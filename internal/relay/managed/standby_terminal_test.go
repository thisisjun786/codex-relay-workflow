package managed

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Expose only Adapter, as the production adapter does: optional Lifecycle must
// not stand in for the hostReady reads exercised by terminal standby recovery.
type terminalStandbyApp struct {
	Adapter
	host   *scriptedApp
	absent bool
	hold   string
}

func (h *terminalStandbyApp) ReadTurn(ctx context.Context, task, turn string) (*Turn, error) {
	if h.absent {
		return nil, nil
	}
	return h.Adapter.ReadTurn(ctx, task, turn)
}

func (h *terminalStandbyApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "thread/list" && h.hold == "recipient_archived" {
		if params["archived"] == true {
			return map[string]any{"data": []any{map[string]any{"id": h.host.order[0]}}}, nil
		}
		return map[string]any{"data": []any{}}, nil
	}
	if method == "thread/goal/get" && h.hold == "recipient_paused" {
		return map[string]any{"goal": map[string]any{"status": "paused"}}, nil
	}
	return h.Adapter.HostCall(ctx, method, params)
}

func terminalStandbyKit(t *testing.T, outcomes ...string) (*reconcileKit, *terminalStandbyApp) {
	t.Helper()
	k := newReconcileKit(t, outcomes...)
	k.host.initialTurns = []string{"standby"}
	h := &terminalStandbyApp{Adapter: k.host, host: k.host}
	k.start.Adapter = h
	if _, ok := k.start.Adapter.(lifecycleChecker); ok {
		t.Fatal("test adapter unexpectedly has Lifecycle")
	}
	return k, h
}

func TestTerminalStandbyContinuesSameRequest(t *testing.T) {
	for _, status := range []string{"completed", "interrupted", "failed"} {
		t.Run(status, func(t *testing.T) {
			k, _ := terminalStandbyKit(t)
			k.host.standby = "inProgress"
			first := k.run()
			k.expect(first, "incomplete", "standby_incomplete", "")
			k.host.standby = status
			// A fresh engine/connection after a restart uses the original request.
			s, err := store.Open(context.Background(), k.start.Store.Path, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			restarted := *k.start
			restarted.Store = s
			k.start = &restarted
			for i := 0; i < 5; i++ {
				got := k.run()
				if got["state"] != "admitted" || got["businessTurnId"] != "business" || got["standbyTurnId"] != "standby" || got["childTaskId"] != first["childTaskId"] {
					t.Fatalf("same request after %s (repeat %d): state=%v reason=%v", status, i, got["state"], got["reason"])
				}
			}
			if k.host.created != 1 || k.host.sent != 1 || !reflect.DeepEqual(k.host.threads["t-1"].turns, []string{"standby", "business"}) {
				t.Fatalf("turns/effects duplicated: %v created=%d sent=%d", k.host.threads["t-1"].turns, k.host.created, k.host.sent)
			}
			var standby, anchor string
			if err := s.DB.QueryRow("SELECT standby_turn_id FROM managed_start_requests WHERE request_id='managed-1'").Scan(&standby); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.QueryRow("SELECT dispatch_turn_id FROM generations WHERE execution_generation=1").Scan(&anchor); err != nil || standby != "standby" || anchor != standby {
				t.Fatalf("anchor changed: standby=%s anchor=%s err=%v", standby, anchor, err)
			}
		})
	}
}

func TestTerminalStandbyRequiresKnownEnd(t *testing.T) {
	for _, status := range []string{"inProgress", "unknown", "", "future", "absent"} {
		t.Run(status, func(t *testing.T) {
			k, h := terminalStandbyKit(t)
			k.host.standby, h.absent = status, status == "absent"
			for i := 0; i < 3; i++ {
				k.expect(k.run(), "incomplete", "standby_incomplete", "")
			}
			if k.host.created != 1 || k.host.sent != 0 {
				t.Fatalf("unobserved end sent business: %d/%d", k.host.created, k.host.sent)
			}
		})
	}
}

func TestTerminalStandbyHostHoldDoesNotConsumeDispatch(t *testing.T) {
	for _, hold := range []string{"recipient_not_idle", "recipient_archived", "recipient_paused", "unreadable"} {
		t.Run(hold, func(t *testing.T) {
			k, h := terminalStandbyKit(t)
			k.host.standby = "inProgress"
			k.expect(k.run(), "incomplete", "standby_incomplete", "")
			k.host.standby, h.hold = "interrupted", hold
			if hold == "recipient_not_idle" {
				k.host.threads["t-1"].status = "active"
			}
			if hold == "unreadable" {
				k.host.failures["thread/read|t-1"] = errors.New("scripted read unavailable")
			}
			for i := 0; i < 3; i++ {
				got, err := k.runErr()
				if hold == "unreadable" {
					if err == nil || err.Error() != "scripted read unavailable" {
						t.Fatalf("read error lost: %v", err)
					}
				} else if err != nil || got["state"] != "incomplete" || got["stage"] != "business" || got["reason"] != hold {
					t.Fatalf("hold %s: %v %v", hold, got, err)
				}
			}
			_, dispatch := OperationIDs("managed-1")
			if k.host.sent != 0 || k.host.operations[dispatch] != nil {
				t.Fatal("known host hold consumed the dispatch operation")
			}
			h.hold, k.host.threads["t-1"].status = "", "idle"
			delete(k.host.failures, "thread/read|t-1")
			if got := k.run(); got["state"] != "admitted" || k.host.created != 1 || k.host.sent != 1 {
				t.Fatalf("cleared hold did not recover: %v", got)
			}
		})
	}
}

func TestTerminalStandbyPolicyPrecedesBusyHost(t *testing.T) {
	k, _ := terminalStandbyKit(t)
	k.host.standby = "inProgress"
	k.run()
	k.host.standby, k.host.threads["t-1"].status = "failed", "active"
	calls := 0
	k.start.Readiness = func(context.Context, map[string]any) (string, error) {
		calls++
		if calls > 1 {
			return "worker_policy_unconfigured", nil
		}
		return "", nil
	}
	k.host.calls = nil
	got := k.run()
	if got["state"] != "refused" || got["stage"] != "business" || got["reason"] != "worker_policy_unconfigured" || k.host.sent != 0 {
		t.Fatalf("policy precedence: %v", got)
	}
	if len(k.host.calls) != 0 {
		t.Fatalf("host readiness ran before the policy refusal: %v", k.host.calls)
	}
}

func TestTerminalStandbyKeepsFinalGuard(t *testing.T) {
	k, _ := terminalStandbyKit(t)
	k.host.standby = "interrupted"
	k.host.beforeSend = func(SendRequest) {
		if _, err := k.start.Store.DB.Exec("UPDATE relationships SET status='paused'"); err != nil {
			t.Fatal(err)
		}
	}
	got := k.run()
	if got["reason"] != "business_failed" || k.host.sent != 0 {
		t.Fatalf("final guard bypassed: %v sent=%d", got, k.host.sent)
	}
}

func TestTerminalStandbyAfterCreationRecovery(t *testing.T) {
	k := newReconcileKit(t, "name-timeout")
	k.start.Adapter = &terminalStandbyApp{Adapter: k.host, host: k.host}
	k.host.standby = "inProgress"
	k.expect(k.run(), "incomplete", "standby_incomplete", "adopted")
	_, business := OperationIDs("managed-1")
	before := k.host.operations[recoveryID("managed-1", 0)]
	k.host.standby = "interrupted"
	for i := 0; i < 3; i++ {
		got := k.run()
		if got["state"] != "admitted" || got["standbyTurnId"] != "recovered-standby" {
			t.Fatalf("recovered standby still held: %v/%v", got["state"], got["reason"])
		}
	}
	if k.host.created != 1 || !reflect.DeepEqual(k.host.sends, []string{recoveryID("managed-1", 0), business}) || !reflect.DeepEqual(before, k.host.operations[recoveryID("managed-1", 0)]) {
		t.Fatalf("creation recovery was repeated: %v", k.host.sends)
	}
	if pyjson.Text(before["turnId"]) != "recovered-standby" {
		t.Fatal("original recovery receipt changed")
	}
}
