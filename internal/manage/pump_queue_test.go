package manage

import (
	"context"
	"encoding/json"
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

// pumpQueueTestPin pins one batch for a thread the way the pump stores it: the logical id, the
// names, the body and each member's digest.
func pumpQueueTestPin(t *testing.T, cfg *Config, thread, logicalID string, names, texts []string) {
	t.Helper()
	st := pumpTestReadState(t, cfg)
	digests := map[string]string{}
	for i, name := range names {
		digests[name] = pumpReview776TestDigest(texts[i])
	}
	st.QueueAttempt[thread] = pumpReview776QueuePin{LogicalID: logicalID, Names: names, Body: pumpReview776QueueBody(texts), SHA256: digests}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
}

// pumpQueueTestSendIDs lists the request ids of the deliveries a fake bridge saw, oldest first. An
// active parent is steered, an idle one is sent, so both tools count.
func pumpQueueTestSendIDs(t *testing.T, log string) []string {
	t.Helper()
	var ids []string
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSend || call["tool"] == deliverToolSteer {
			ids = append(ids, deliverSendRequestIDOf(t, call))
		}
	}
	return ids
}

// pumpQueueTestRefusals reads the refusal counts the state document holds for each thread.
func pumpQueueTestRefusals(t *testing.T, cfg *Config) map[string]pumpQueueTestCount {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		QueueRefused map[string]pumpQueueTestCount `json:"queue_refused"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.QueueRefused
}

// pumpQueueTestCount is one thread's refusal count as the state document stores it.
type pumpQueueTestCount struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// A notice the ledger already settled as accepted completes its pin without asking the bridge,
// so the move happens even when the bridge cannot be reached.
func TestPumpQueueLocalAcceptedRecordCompletesWithoutTheBridge(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	pumpQueueTestPin(t, cfg, "parent-1", "pinid", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"})
	if err := deliverSave(cfg, deliverRecord{LogicalID: "pinid", RequestID: "pinid", Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"a-body"})), CreatedAt: pumpTestNow.UTC().Format(time.RFC3339), State: deliverStateAccepted}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the accepted record did not complete its pinned notice: %v", err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("the accepted record left the pin")
	}
}

// A notice the ledger already settled as refused lifts its pin without asking the bridge.
func TestPumpQueueLocalRefusedRecordLiftsThePinWithoutTheBridge(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	pumpQueueTestPin(t, cfg, "parent-1", "pinid", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"})
	if err := deliverSave(cfg, deliverRecord{LogicalID: "pinid", RequestID: "pinid", Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"a-body"})), CreatedAt: pumpTestNow.UTC().Format(time.RFC3339), State: deliverStateRefused}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("the refused record kept the pin")
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 {
		t.Errorf("the refused record moved the notice: %v", names)
	}
}

// An idle parent is judged on the whole queue: an older notice outside the size-cut prefix makes
// the queue due, and the prefix that fits is what the batch carries.
func TestPumpQueueIdleTimeoutJudgesTheWholeQueue(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	big := strings.Repeat("x", 46000)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	fresh := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", big)
	old := pumpQueueTestNotice(t, cfg, "parent-1", "zzzzzzzzzzzzzzzz.txt", big)
	if err := os.Chtimes(fresh, pumpTestNow, pumpTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, pumpTestNow.Add(-2*time.Hour), pumpTestNow.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the older notice outside the prefix did not make the idle queue due: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "zzzzzzzzzzzzzzzz.txt" {
		t.Errorf("the queue after the first round = %v, want the older notice only", names)
	}
}

// A refused batch is sent again under the next ordinal of its id, and the third refusal of the
// same batch moves its notices to refused/ with a log line, so the notices behind it flow.
func TestPumpQueueRefusedBatchRetriesWithOrdinalThenMovesAside(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, pumpStateFile), []byte(`{"future_key": {"keep": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for round := 0; round < 3; round++ {
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
			{"payload": map[string]any{"status": "refused"}},
		})
		cfg.Bridge.Binary = bridge
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
		sent := pumpQueueTestSendIDs(t, log)
		if len(sent) != 1 {
			t.Fatalf("round %d sent %v, want one delivery; bridge calls %v", round+1, sent, deliverSendToolsOf(t, log))
		}
		ids = append(ids, sent[0])
	}
	base := ids[0]
	if ids[1] != base+"-r1" || ids[2] != base+"-r2" {
		t.Errorf("the refused batch was sent under %v, want %s then -r1 then -r2", ids, base)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("the refused batch kept its pin")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", "refused", "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the third refusal did not move the notice to refused/: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 0 {
		t.Errorf("the queue still holds %v after the move", names)
	}
	if refusals := pumpQueueTestRefusals(t, cfg); len(refusals) != 0 {
		t.Errorf("the refusal count survived the move: %v", refusals)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "aaaaaaaaaaaaaaaa.txt") || !strings.Contains(string(raw), "6 bytes") {
		t.Errorf("the refusal log line is missing the name and size: %q", string(raw))
	}
	stateRaw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stateRaw), "future_key") {
		t.Errorf("the state write dropped an unknown key")
	}
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "b-body")
	okBridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg.Bridge.Binary = okBridge
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "bbbbbbbbbbbbbbbb.txt")); err != nil {
		t.Errorf("the notice behind the moved batch did not flow: %v", err)
	}
}

// A legacy ledger record whose names are the oldest notice alone is found although the name-ordered
// prefix is a different set, so the settled attempt is reconciled and not sent again under a new id.
func TestPumpQueueLegacyRecordMatchesTheOldestSetNotOnlyThePrefix(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	old := pumpQueueTestNotice(t, cfg, "parent-1", "zzzzzzzzzzzzzzzz.txt", "z-body")
	if err := os.Chtimes(old, pumpTestNow.Add(-time.Hour), pumpTestNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	legacyID := pumpQueueLegacyBatchID("parent-1", []string{"zzzzzzzzzzzzzzzz.txt"})
	if err := deliverSave(cfg, deliverRecord{LogicalID: legacyID, RequestID: legacyID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"z-body"})), CreatedAt: pumpTestNow.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Fatalf("a settled attempt was sent again under a new id: %v", deliverSendToolsOf(t, log))
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "zzzzzzzzzzzzzzzz.txt")); err != nil {
		t.Errorf("the reconciled notice was not completed: %v", err)
	}
}

// A legacy record written before the oldest-first member was modified cannot have covered it, so the
// record is not adopted and the batch takes its current id with every notice in it.
func TestPumpQueueLegacyRecordWrittenBeforeAMemberIsNotAdopted(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	old := pumpQueueTestNotice(t, cfg, "parent-1", "zzzzzzzzzzzzzzzz.txt", "z-body")
	if err := os.Chtimes(old, pumpTestNow.Add(-time.Hour), pumpTestNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	legacyID := pumpQueueLegacyBatchID("parent-1", []string{"zzzzzzzzzzzzzzzz.txt"})
	if err := deliverSave(cfg, deliverRecord{LogicalID: legacyID, RequestID: legacyID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"z-body"})), CreatedAt: pumpTestNow.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	sentIDs := pumpQueueTestSendIDs(t, log)
	if len(sentIDs) != 1 || sentIDs[0] == legacyID {
		t.Errorf("the batch was sent under %v, want one new id", sentIDs)
	}
	for _, name := range []string{"aaaaaaaaaaaaaaaa.txt", "zzzzzzzzzzzzzzzz.txt"} {
		if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, name)); err != nil {
			t.Errorf("the notice %s was not delivered: %v", name, err)
		}
	}
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
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
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
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
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
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
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
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
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
