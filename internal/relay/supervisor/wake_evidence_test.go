package supervisor

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func TestFlowSendRetainsWakeAttempt(t *testing.T) {
	for _, status := range []string{"idle", "notLoaded", "active", "unknown-response"} {
		t.Run(status, func(t *testing.T) {
			f := flowRecordFixture(t)
			id := stageFlowWake(t, f)
			_, host := flowSteerHost(t)
			host.status = status
			if status == "unknown-response" {
				host.status, host.outcome = "idle", "unknown"
			}
			result, err := f.c.attempt(f.ctx, id, host, flowSteerNow, 0, "relay-daemon", daemonAttempt)
			if err != nil || result == nil {
				t.Fatalf("send: %v %v", result, err)
			}
			var raw string
			if err := f.s.DB.QueryRowContext(f.ctx, `SELECT detail FROM journal WHERE kind=? AND subject=?`, delivery.WakeObserved, result["requestId"]).Scan(&raw); err != nil {
				t.Fatalf("real supervisor send lost wake evidence: %v", err)
			}
			var record struct {
				delivery.WakeAttempt
				Method string `json:"delivery_method"`
			}
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				t.Fatal(err)
			}
			method := "turn/start"
			if status == "active" {
				method = "turn/steer"
			}
			if record.RequestID != result["requestId"] || record.RecipientStatusBefore != host.status || record.Method != method || !record.Unattended {
				t.Fatalf("wrong attempt evidence: %s", raw)
			}
			if (status == "active" || status == "unknown-response") && record.NewTurn {
				t.Fatalf("arrival or uncertainty counted as wake: %s", raw)
			}
			if _, err := f.c.Attempt(f.ctx, id, host, flowSteerNow+1); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.s.DB.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM journal WHERE kind=?`, delivery.WakeObserved).Scan(&count); err != nil || count != 1 {
				t.Fatalf("attempt duplicated: %d %v", count, err)
			}
		})
	}
}
