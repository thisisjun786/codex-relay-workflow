package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pumpQueueTestNotice writes one queued notice for a thread and returns its path.
func pumpQueueTestNotice(t *testing.T, cfg *Config, thread, name, text string) string {
	t.Helper()
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pumpQueueTestNames lists the *.txt notices still queued for a thread.
func pumpQueueTestNames(t *testing.T, cfg *Config, thread string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, pumpQueueDir, thread))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".txt") {
			out = append(out, entry.Name())
		}
	}
	return out
}

// An active parent has all queued notices steered in one batch.
func TestPumpQueueActiveSteersAll(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	// The probe and Deliver each start their own bridge process, and the fake scenario counter
	// restarts per process: the probe reads step 0, then Deliver reads step 0 and step 1.
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "one")
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "two")
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	tools := deliverSendToolsOf(t, log)
	if len(tools) == 0 || tools[len(tools)-1] != deliverToolSteer {
		t.Fatalf("calls = %v, want a steer as the last call", tools)
	}
	for _, tool := range tools {
		if tool == deliverToolSend {
			t.Fatalf("an active parent was sent a new turn instead of a steer: %v", tools)
		}
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 0 {
		t.Errorf("accepted left %v queued", names)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir)); err != nil {
		t.Errorf("the sent directory: %v", err)
	}
}

// An idle parent that has not waited past max_queue_seconds is left alone.
func TestPumpQueueWaitsUntilMax(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "one")
	if err := os.Chtimes(path, pumpTestNow, pumpTestNow); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Fatalf("a wait sent something: %v", deliverSendToolsOf(t, log))
		}
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 {
		t.Errorf("the wait moved the notice: %v", names)
	}
}

// An idle parent past max_queue_seconds is sent the batch with the parent role.
func TestPumpQueueSendsRoleParentAfterMax(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "one")
	old := pumpTestNow.Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, log)
	if len(calls) == 0 || calls[len(calls)-1]["tool"] != deliverToolSend {
		t.Fatalf("calls = %v, want a send as the last call", deliverSendToolsOf(t, log))
	}
	args, _ := calls[len(calls)-1]["args"].(map[string]any)
	if args["role"] != "parent" {
		t.Errorf("role = %v, want parent", args["role"])
	}
}

// An unknown outcome leaves the queued files in place.
func TestPumpQueueUnknownKeepsFiles(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "one")
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 {
		t.Errorf("unknown moved the notice: %v", names)
	}
}

// The queue batch id is stable across rounds for the same file names.
func TestPumpQueueBatchIDIsStable(t *testing.T) {
	a := pumpBatchIDStrings([]string{"aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"})
	b := pumpBatchIDStrings([]string{"aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"})
	if a != b || len(a) != 16 {
		t.Fatalf("queue batch id = %q then %q, want the same 16 characters", a, b)
	}
	if pumpBatchIDStrings([]string{"aaaaaaaaaaaaaaaa.txt"}) == a {
		t.Error("a different file set produced the same id")
	}
}

// The batch id is stable for the same events and different for a different set.
func TestPumpBatchIDIsStable(t *testing.T) {
	events := []pumpEvent{{ID: "a"}, {ID: "b"}}
	if pumpBatchID(events) != pumpBatchID([]pumpEvent{{ID: "a"}, {ID: "b"}}) {
		t.Error("the same events produced different ids")
	}
	// The newline-in-id collision the architect named is impossible with the JSON array.
	if pumpBatchID([]pumpEvent{{ID: "a\nb"}}) == pumpBatchID([]pumpEvent{{ID: "a"}, {ID: "b"}}) {
		t.Error("the id encoding is ambiguous")
	}
}
