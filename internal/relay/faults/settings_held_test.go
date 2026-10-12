package faults

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1274: a delivery held on a settings refusal no retry fixes (held_settings_mismatch) still raises the delivery_stalled fault,
// with the settings cause in its evidence.
func TestDeliveryHeldOnSettingsMismatchStillRaisesDeliveryStalled(t *testing.T) {
	goldenParent(t)
	home := t.TempDir()
	s, err := store.Open(context.Background(), filepath.Join(home, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, statement := range []string{
		`INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,hold_reason,attempt_count,created_at,updated_at) VALUES('evt1','rel','completion','parent','thread','withheld_pre_send','held_settings_mismatch',1,'now','now')`,
		`INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('req1','evt1',1,'completion','settled','withheld_pre_send','now')`,
		`INSERT INTO journal(at,kind,subject,detail) VALUES('now','delivery_attempted','evt1','{"requestId":"req1","hold":"held_settings_mismatch","settingsRefusal":{"reason":"settings_not_preserved","field":"model","expected":"anthropic/claude-sonnet-5-5","observed":"anthropic/claude-opus-5-5"}}')`,
	} {
		if _, err := s.Q(context.Background()).ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	sw := &Sweeper{Store: s, HostRecordPath: testHostRecordPath(), MaxAttempts: 6, Installation: Installation{Package: "codex-session-relay", Version: "test", Location: "test"}, Program: func() []string { return []string{"codex-session-relay"} }}
	got, err := sw.deliveryFaults(context.Background(), "crw", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got.observations)
	if err != nil {
		t.Fatal(err)
	}
	var observed []map[string]any
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0]["FaultClass"] != "delivery_stalled" {
		t.Fatalf("observations = %s, want one delivery_stalled", raw)
	}
	if detail, _ := observed[0]["Detail"].(string); !strings.Contains(detail, "held_settings_mismatch") || !strings.Contains(detail, "settings_not_preserved") {
		t.Fatalf("detail = %q, want the hold and the settings cause", detail)
	}
}
