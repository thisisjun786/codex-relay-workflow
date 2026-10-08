package manage

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// A refusal the bridge's receipt settles is counted once: the pin is lifted by the reconcile, and the
// round must not count the same refusal again through the ledger's replayed answer.
func TestPumpQueueReceiptRefusalIsCountedOnce(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"status": "refused", "observation": "idle"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	if err := os.Chtimes(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", "aaaaaaaaaaaaaaaa.txt"), pumpTestNow.Add(-2*time.Hour), pumpTestNow.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestPin(t, cfg, "parent-1", "pinid", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"})
	if err := deliverSave(cfg, deliverRecord{LogicalID: "pinid", RequestID: "pinid", Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"a-body"})), CreatedAt: pumpTestNow.Add(-time.Minute).UTC().Format(time.RFC3339),
		State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if refusals := pumpQueueTestRefusals(t, cfg); refusals["parent-1"].Count != 1 {
		t.Errorf("the receipt refusal was counted %d times, want once: %v", refusals["parent-1"].Count, refusals)
	}
}

// The idle-parent gate of a pinned retry judges what the next batch would carry: a notice the queue
// would move aside as oversize does not age the pin into a send.
func TestPumpQueueRetryGateIgnoresNoticesMovedAsideAsOversize(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
	})
	cfg := pumpTestConfig(t, bridge)
	fresh := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	huge := pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", strings.Repeat("y", 130000))
	if err := os.Chtimes(fresh, pumpTestNow, pumpTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(huge, pumpTestNow.Add(-2*time.Hour), pumpTestNow.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestPin(t, cfg, "parent-1", "pinid", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"})
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Fatalf("an oversize notice aged the pin into a send: %v", deliverSendToolsOf(t, log))
		}
	}
}

// pumpQueueTestStamp is a ledger stamp the way Deliver writes one: whole seconds, UTC.
func pumpQueueTestStamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// pumpQueueTestSetTime sets a queued notice's modification time.
func pumpQueueTestSetTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// pumpQueueTestBackdate makes every queued notice of a thread older than the ledger records the
// legacy fixtures store at the clock's now, as a notice that was queued before the attempt is.
func pumpQueueTestBackdate(t *testing.T, cfg *Config, thread string) {
	t.Helper()
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	for _, name := range pumpQueueTestNames(t, cfg, thread) {
		pumpQueueTestSetTime(t, filepath.Join(dir, name), pumpTestNow.Add(-time.Hour))
	}
}

// pumpQueueTestLegacyRecord stores a pre-change ledger record under the pre-change id of the names.
func pumpQueueTestLegacyRecord(t *testing.T, cfg *Config, thread string, names, texts []string, createdAt time.Time, state string) string {
	t.Helper()
	legacyID := pumpQueueLegacyBatchID(thread, names)
	if err := deliverSave(cfg, deliverRecord{LogicalID: legacyID, RequestID: legacyID, Tool: deliverToolSend, TargetThread: thread,
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody(texts)), CreatedAt: pumpQueueTestStamp(createdAt), State: state}); err != nil {
		t.Fatal(err)
	}
	return legacyID
}

// A legacy record written before a name-ordered member was last modified is not that attempt: the
// member came after it, so the record is not adopted and the batch takes its own id.
func TestPumpQueueStaleNameOrderedLegacyRecordIsNotAdopted(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	pumpQueueTestSetTime(t, path, pumpTestNow.Add(-time.Hour))
	legacyID := pumpQueueTestLegacyRecord(t, cfg, "parent-1", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"}, pumpTestNow.Add(-2*time.Hour), deliverStateUnknown)
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	sent := pumpQueueTestSendIDs(t, log)
	if len(sent) != 1 || sent[0] == legacyID {
		t.Errorf("the batch was sent under %v, want one new id and not the stale legacy id %s", sent, legacyID)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the notice was not delivered: %v", err)
	}
}

// A stale accepted record whose text is not the text on disk must not become a Held pin: the notice
// was written after the record, so nothing says the attempt carried it, and a held pin would block
// every notice added behind it.
func TestPumpQueueStaleAcceptedLegacyRecordDoesNotHoldTheThread(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "the new body")
	pumpQueueTestSetTime(t, path, pumpTestNow.Add(-time.Hour))
	legacyID := pumpQueueTestLegacyRecord(t, cfg, "parent-1", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"the old body"}, pumpTestNow.Add(-2*time.Hour), deliverStateAccepted)
	other := pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "b-body")
	pumpQueueTestSetTime(t, other, pumpTestNow.Add(-time.Hour))
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok && pin["held"] == true {
		t.Errorf("a stale accepted record held the thread: %v", pin)
	}
	sent := pumpQueueTestSendIDs(t, log)
	if len(sent) != 1 || sent[0] == legacyID {
		t.Errorf("the batch was sent under %v, want one new id", sent)
	}
	for _, name := range []string{"aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"} {
		if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, name)); err != nil {
			t.Errorf("the notice %s was not delivered: %v", name, err)
		}
	}
}

// The ledger stamp has whole seconds while a file's modification time has nanoseconds. A notice
// written half a second into the stamp's own second was there when the attempt was made, so its
// record is adopted for a name-ordered set and for an oldest-first set alike.
func TestPumpQueueLegacyRecordInTheSameSecondAsAMemberIsAdopted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		others  bool
		members []string
	}{
		{"name-ordered", false, []string{"aaaaaaaaaaaaaaaa.txt"}},
		{"oldest-first", true, []string{"zzzzzzzzzzzzzzzz.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, log := deliverFakeBridge(t, []map[string]any{})
			cfg := pumpTestConfig(t, bridge)
			stamp := pumpTestNow.Add(-time.Hour)
			texts := make([]string, len(tc.members))
			for i, member := range tc.members {
				texts[i] = member[:1] + "-body"
				pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, "parent-1", member, texts[i]), stamp.Add(500*time.Millisecond))
			}
			if tc.others {
				// A notice queued after the attempt sorts before the attempt's own member.
				pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body"), stamp.Add(time.Minute))
			}
			pumpQueueTestLegacyRecord(t, cfg, "parent-1", tc.members, texts, stamp, deliverStateAccepted)
			if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
				t.Fatal(err)
			}
			if sent := pumpQueueTestSendIDs(t, log); len(sent) != 0 {
				t.Errorf("an attempt made in the same second was sent again under %v", sent)
			}
			for _, member := range tc.members {
				if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, member)); err != nil {
					t.Errorf("the notice %s the attempt carried was not completed: %v", member, err)
				}
			}
		})
	}
}

// A member modified in a later second than the stamp is newer than the attempt.
func TestPumpQueueLegacyRecordBeforeALaterSecondIsNotAdopted(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	stamp := pumpTestNow.Add(-time.Hour)
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body"), stamp.Add(1500*time.Millisecond))
	legacyID := pumpQueueTestLegacyRecord(t, cfg, "parent-1", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"}, stamp, deliverStateAccepted)
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if sent := pumpQueueTestSendIDs(t, log); len(sent) != 1 || sent[0] == legacyID {
		t.Errorf("a notice from a later second was sent under %v, want one new id", sent)
	}
}

// pumpQueueCancelCtx reports a cancellation from its (after+1)-th Err call on, and runs onCancel the
// moment it first reports one, so a test can see what the round changed after that point.
type pumpQueueCancelCtx struct {
	context.Context
	after, calls int
	cancelled    bool
	onCancel     func()
}

func (c *pumpQueueCancelCtx) Err() error {
	c.calls++
	if c.calls <= c.after {
		return nil
	}
	if !c.cancelled {
		c.cancelled = true
		c.onCancel()
	}
	return context.Canceled
}

// pumpQueueTreeSnapshot is every file under dir (except the log) with its content, for comparing two
// moments of a state directory.
func pumpQueueTreeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == pumpLogFile {
			return nil
		}
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A round cancelled at any point of its recovery and adoption steps makes no durable change after the
// cancellation: the state directory is the same when the round returns as when it was first told to
// stop, and the state the round still holds in memory is the state on disk.
func TestPumpQueueRoundCancelledMidwayChangesNothingAfterwards(t *testing.T) {
	scenarios := []struct {
		name  string
		setup func(t *testing.T, cfg *Config)
	}{
		{"legacy accepted record", func(t *testing.T, cfg *Config) {
			path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
			pumpQueueTestSetTime(t, path, pumpTestNow.Add(-time.Hour))
			pumpQueueTestLegacyRecord(t, cfg, "parent-1", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"}, pumpTestNow.Add(-30*time.Minute), deliverStateAccepted)
		}},
		{"legacy held record", func(t *testing.T, cfg *Config) {
			path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "new body")
			pumpQueueTestSetTime(t, path, pumpTestNow.Add(-time.Hour))
			pumpQueueTestLegacyRecord(t, cfg, "parent-1", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"old body"}, pumpTestNow.Add(-30*time.Minute), deliverStateAccepted)
		}},
		{"legacy unknown record", func(t *testing.T, cfg *Config) {
			path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
			pumpQueueTestSetTime(t, path, pumpTestNow.Add(-time.Hour))
			pumpQueueTestLegacyRecord(t, cfg, "parent-1", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"}, pumpTestNow.Add(-30*time.Minute), deliverStateUnknown)
		}},
		{"aside left by an interrupted move", func(t *testing.T, cfg *Config) {
			dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
			aside := filepath.Join(dir, pumpReview776AsideDir)
			if err := os.MkdirAll(aside, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(aside, "aaaaaaaaaaaaaaaa.txt"), []byte("a-body"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"refused pin", func(t *testing.T, cfg *Config) {
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body"), pumpTestNow.Add(-time.Hour))
			pumpQueueTestPin(t, cfg, "parent-1", "pinid", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"})
			if err := deliverSave(cfg, deliverRecord{LogicalID: "pinid", RequestID: "pinid", Tool: deliverToolSend, TargetThread: "parent-1",
				MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"a-body"})), CreatedAt: pumpQueueTestStamp(pumpTestNow.Add(-30 * time.Minute)), State: deliverStateRefused}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			cancelledRuns := 0
			for after := 0; after < 40; after++ {
				now := pumpTestNow
				e := pumpTestEnv(t, &now)
				cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
				sc.setup(t, cfg)
				st := pumpTestReadStatePtr(t, cfg)
				var atCancel map[string]string
				ctx := &pumpQueueCancelCtx{Context: context.Background(), after: after}
				ctx.onCancel = func() { atCancel = pumpQueueTreeSnapshot(t, cfg.StateDir) }
				if err := pumpQueueFlush(ctx, e, cfg, st, pumpSettingsFrom(cfg), false); err != nil {
					t.Fatal(err)
				}
				if !ctx.cancelled {
					break
				}
				cancelledRuns++
				final := pumpQueueTreeSnapshot(t, cfg.StateDir)
				var diff []string
				for path, content := range final {
					if before, ok := atCancel[path]; !ok || before != content {
						diff = append(diff, path)
					}
				}
				for path := range atCancel {
					if _, ok := final[path]; !ok {
						diff = append(diff, path)
					}
				}
				sort.Strings(diff)
				if len(diff) > 0 {
					t.Errorf("cancelled at Err call %d: the round changed %v afterwards", after+1, diff)
				}
				disk := pumpTestReadState(t, cfg)
				if len(disk.QueueAttempt) != len(st.QueueAttempt) {
					t.Errorf("cancelled at Err call %d: the round holds %d pins in memory, the state file %d", after+1, len(st.QueueAttempt), len(disk.QueueAttempt))
				}
			}
			if cancelledRuns == 0 {
				t.Fatalf("no run was cancelled; the sweep exercised nothing")
			}
		})
	}
}

// A crash between a third refusal's moves and the state save leaves the pin and the count behind
// with the notices already in refused/. The next round converges: the pin lifts, the count clears
// and a notice queued behind flows, without moving or counting anything twice.
func TestPumpQueueRefusedMovesThenStoreCrashConverges(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(filepath.Join(dir, pumpQueueRefusedDir), 0o700); err != nil {
		t.Fatal(err)
	}
	// The earlier round moved the refused notice and died before it saved: the file is in refused/
	// and the state still holds the pin and the second refusal.
	if err := os.WriteFile(filepath.Join(dir, pumpQueueRefusedDir, "aaaaaaaaaaaaaaaa.txt"), []byte("a-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestPin(t, cfg, "parent-1", "pinid-r2", []string{"aaaaaaaaaaaaaaaa.txt"}, []string{"a-body"})
	st := pumpTestReadStatePtr(t, cfg)
	pin := st.QueueAttempt["parent-1"]
	pin.Base = "pinid"
	st.QueueAttempt["parent-1"] = pin
	st.QueueRefused["parent-1"] = pumpQueueRefusal{ID: "pinid", Count: 2}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	if err := deliverSave(cfg, deliverRecord{LogicalID: "pinid-r2", RequestID: "pinid-r2", Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(pumpReview776QueueBody([]string{"a-body"})), CreatedAt: pumpQueueTestStamp(pumpTestNow.Add(-time.Hour)), State: deliverStateRefused}); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "b-body"), pumpTestNow.Add(-time.Minute))
	for round := 0; round < 2; round++ {
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("the pin survived the rerun")
	}
	if refusals := pumpQueueTestRefusals(t, cfg); len(refusals) != 0 {
		t.Errorf("the refusal count survived the rerun: %v", refusals)
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir, "bbbbbbbbbbbbbbbb.txt")); err != nil {
		t.Errorf("the notice behind the refused batch did not flow: %v (sends %v)", err, pumpQueueTestSendIDs(t, log))
	}
	if entries, err := os.ReadDir(filepath.Join(dir, pumpQueueRefusedDir)); err != nil || len(entries) != 1 {
		t.Errorf("refused/ holds %v (%v), want the one earlier notice", entries, err)
	}
}
