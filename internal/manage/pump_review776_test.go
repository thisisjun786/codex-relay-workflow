package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The review-776 tests drive the five P1 defects of the ported event pump through the same fake
// bridge, injected clock and temporary state directory the pump tests use, so no test reaches a
// real bridge, the relay store, the network or a real state directory. The pin is read from the
// state document as JSON rather than through a struct field, so the file compiles and fails on
// the unfixed tree too, which is what makes the red evidence meaningful.

// pumpReview776Tree lists a directory tree as sorted "relative-path size" lines, so a test can
// compare a tree before and after a run byte for byte.
func pumpReview776Tree(t *testing.T, root string) string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, fmt.Sprintf("%s %d", rel, info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// pumpReview776QueueAttempt is the pinned queue batch for one thread, read from the state
// document, and whether the document carries one.
func pumpReview776QueueAttempt(t *testing.T, cfg *Config, thread string) (map[string]any, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		QueueAttempt map[string]map[string]any `json:"queue_attempt"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	pin, ok := doc.QueueAttempt[thread]
	return pin, ok
}

// An unknown queue delivery pins the batch\'s names, body and logical id, and the next round
// reconciles that same logical id with that same body even though a new notice arrived.
func TestPumpReview776UnknownQueueBatchIsPinned(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")

	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	pin, ok := pumpReview776QueueAttempt(t, cfg, "parent-1")
	if !ok {
		t.Fatalf("an unknown queue delivery pinned nothing")
	}
	want, _ := pin["logical_id"].(string)
	if want == "" {
		t.Fatalf("the pin carries no logical id: %v", pin)
	}
	names, _ := pin["names"].([]any)
	body, _ := pin["body"].(string)
	if len(names) != 1 || names[0] != "aaaaaaaaaaaaaaaa.txt" || !strings.Contains(body, "a-body") {
		t.Errorf("pinned batch = %v, want the one notice and its body", pin)
	}

	// A second notice arrives before the next round.
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "b-body")

	// The retry round runs its own bridge, because the fake scenario counter restarts with each
	// process: the queue probe and the reconcile both read the first step, then the send's own
	// probe and receipt follow. The first step answers both readings: an active turn with an
	// operation the bridge recorded but never attempted, which reconciles under the same id.
	retryBridge, retryLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1", "status": "not_attempted"}},
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg.Bridge.Binary = retryBridge
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, retryLog)
	reconciled := false
	for _, call := range calls {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == want {
			reconciled = true
		}
	}
	if !reconciled {
		t.Fatalf("the second round did not reconcile the pinned id %q: %v", want, deliverSendToolsOf(t, retryLog))
	}
	last := calls[len(calls)-1]
	args, _ := last["args"].(map[string]any)
	message, _ := args["message"].(string)
	if !strings.Contains(message, "a-body") || strings.Contains(message, "b-body") {
		t.Errorf("the retry carried %q, want the pinned body only", message)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the pinned notice was not completed: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "bbbbbbbbbbbbbbbb.txt" {
		t.Errorf("the queue after the retry = %v, want the new notice only", names)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("the pin survived the accepted retry")
	}
}

// A refused queue delivery lifts the pin and leaves the notices queued.
func TestPumpReview776RefusedQueueBatchClearsPin(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "refused"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("a refused delivery kept the pin")
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 {
		t.Errorf("a refused delivery moved the notice: %v", names)
	}
}

// A pin whose notices are all gone is dropped, and the thread starts no bridge process for it.
func TestPumpReview776StaleQueuePinIsDropped(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{})
	cfg := pumpTestConfig(t, bridge)
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"queue_attempt": map[string]any{"parent-1": map[string]any{
		"logical_id": "abc123", "names": []string{"aaaaaaaaaaaaaaaa.txt"}, "body": "gone"}}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, pumpStateFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Errorf("the stale pin survived")
	}
	if tools := deliverSendToolsOf(t, log); len(tools) != 0 {
		t.Errorf("the stale pin started a bridge process: %v", tools)
	}
}

// A repeated PR transition is a new event, so the sent set cannot swallow it.
func TestPumpReview776RepeatedPRTransitionIsNew(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Repository = "owner/repo"
	saved := pumpExec
	t.Cleanup(func() { pumpExec = saved })
	pumpTestSwapSources(t, pumpPRSource{})
	st := pumpTestReadState(t, cfg)
	collect := func(state string) {
		out, _ := json.Marshal([]map[string]any{{"number": 12, "state": state, "title": "CRW-1: x"}})
		pumpExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return out, nil }
		pumpCollect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg), now)
	}
	deliver := func(what string) string {
		if len(st.Pending) != 1 {
			t.Fatalf("%s produced pending %+v, want one event", what, st.Pending)
		}
		id := st.Pending[0].ID
		st.Sent[id] = true
		st.Pending = nil
		return id
	}
	collect("OPEN")
	if len(st.Pending) != 0 {
		t.Fatalf("the first sight produced %+v, want the baseline only", st.Pending)
	}
	collect("CLOSED")
	firstClosed := deliver("the closure")
	collect("OPEN")
	deliver("the reopening")
	collect("CLOSED")
	secondClosed := deliver("the repeated closure")
	if firstClosed == secondClosed {
		t.Fatalf("the repeated closure reused the event id %q", secondClosed)
	}
}

// --dry-run changes nothing for the whole round: no file moves and no state write.
func TestPumpReview776DryRunChangesNothing(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake"})
	notice := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	st := pumpTestReadState(t, cfg)
	st.QueueAccepted["parent-1"] = []string{"aaaaaaaaaaaaaaaa.txt"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.StateDir, pumpStateFile)
	queueRoot := filepath.Join(cfg.StateDir, pumpQueueDir)
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTree := pumpReview776Tree(t, queueRoot)

	var out strings.Builder
	e.Stdout = &out
	if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), true); code != 0 || err != nil {
		t.Fatalf("the dry round: code=%d err=%v", code, err)
	}
	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Errorf("the dry run rewrote the state file:\n%s\n%s", beforeState, afterState)
	}
	if afterTree := pumpReview776Tree(t, queueRoot); afterTree != beforeTree {
		t.Errorf("the dry run changed the queue:\n%s\n%s", beforeTree, afterTree)
	}
	if _, err := os.Stat(notice); err != nil {
		t.Errorf("the dry run moved the notice: %v", err)
	}
	if _, err := os.Stat(filepath.Join(queueRoot, "parent-1", pumpSentDir)); !os.IsNotExist(err) {
		t.Errorf("the dry run created sent/: %v", err)
	}
	if !strings.Contains(out.String(), "would complete 1 accepted notices") {
		t.Errorf("the dry run printed %q, want the would-complete line", out.String())
	}
}

// A parent-queue batch is the longest name-ordered prefix that fits the batch limit, and a single
// notice that alone exceeds it is moved aside so the rest of the queue still flows.
func TestPumpReview776QueueSplitsBySize(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	big := strings.Repeat("x", 60000)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", big)
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", big)
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir)
	if _, err := os.Stat(filepath.Join(sent, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the first round did not deliver the first notice: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "bbbbbbbbbbbbbbbb.txt" {
		t.Fatalf("the first round left %v, want the second notice only", names)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sent, "bbbbbbbbbbbbbbbb.txt")); err != nil {
		t.Errorf("the second round did not deliver the rest: %v", err)
	}

	// A single notice over the limit is moved to oversize/ and the rest still goes.
	now2 := pumpTestNow
	e2 := pumpTestEnv(t, &now2)
	bridge2, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg2 := pumpTestConfig(t, bridge2)
	huge := strings.Repeat("y", 130000)
	pumpQueueTestNotice(t, cfg2, "parent-1", "aaaaaaaaaaaaaaaa.txt", huge)
	pumpQueueTestNotice(t, cfg2, "parent-1", "cccccccccccccccc.txt", "small")
	if err := pumpQueueFlush(context.Background(), e2, cfg2, pumpTestReadStatePtr(t, cfg2), pumpSettingsFrom(cfg2), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg2.StateDir, pumpQueueDir, "parent-1", pumpReview776OversizeDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the oversize notice was not moved aside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg2.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "cccccccccccccccc.txt")); err != nil {
		t.Errorf("the fitting notice was not delivered: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg2, "parent-1"); len(names) != 0 {
		t.Errorf("the queue still holds %v", names)
	}
	raw, err := os.ReadFile(filepath.Join(cfg2.StateDir, pumpLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "aaaaaaaaaaaaaaaa.txt") || !strings.Contains(string(raw), "130000") {
		t.Errorf("the oversize log line is missing: %q", string(raw))
	}
}

// An urgent round delivers the urgent events first, so a question collected after a burst of
// reports still lands in the first batch.
func TestPumpReview776UrgentGoesFirst(t *testing.T) {
	events := make([]pumpEvent, 0, 17)
	for i := 0; i < 16; i++ {
		events = append(events, pumpEvent{ID: fmt.Sprintf("r%d", i), Kind: pumpKindReport, Text: strings.Repeat("x", 6000)})
	}
	events = append(events, pumpEvent{ID: "q", Kind: pumpKindQuestion, Text: "the urgent question"})
	batch := pumpBatchPrefix(events, pumpTestNow, true, "")
	if len(batch) == 0 || batch[0].ID != "q" {
		t.Fatalf("the first batch starts with %+v, want the urgent question first", batch[0])
	}
	again := pumpBatchPrefix(events, pumpTestNow, true, "")
	if pumpBatchID(batch) != pumpBatchID(again) {
		t.Error("the urgent order is not stable for the same pending set")
	}
}

// A retry that hits a local error learns nothing about the pinned attempt, so the pin stays.
func TestPumpReview776RetryKeepsPinOnLocalError(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	st := pumpTestReadState(t, cfg)
	st.QueueAttempt["parent-1"] = pumpReview776QueuePin{LogicalID: "pinid", Names: []string{"aaaaaaaaaaaaaaaa.txt"}, Body: "a-body"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	_ = path
	// Hold the delivery lock so the retry's Deliver fails before it dials the bridge.
	release, err := deliverLock(cfg, "pinid")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); !ok {
		t.Error("a local retry error dropped the pin")
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Errorf("a failed retry sent something: %v", deliverSendToolsOf(t, log))
		}
	}
}

// A pinned retry keeps the queue's own idle timeout: it does not open a parent turn early.
func TestPumpReview776RetryWaitsForIdleTimeout(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	if err := os.Chtimes(path, pumpTestNow, pumpTestNow); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadState(t, cfg)
	st.QueueAttempt["parent-1"] = pumpReview776QueuePin{LogicalID: "pinid", Names: []string{"aaaaaaaaaaaaaaaa.txt"}, Body: "a-body"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Fatalf("a fresh pin opened a turn early: %v", deliverSendToolsOf(t, log))
		}
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); !ok {
		t.Error("the wait dropped the pin")
	}
	// Once the pinned notice is old enough, the retry sends it.
	old := pumpTestNow.Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	retryBridge, retryLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg.Bridge.Binary = retryBridge
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, retryLog)
	if len(calls) == 0 || calls[len(calls)-1]["tool"] != deliverToolSend {
		t.Fatalf("the aged pin did not send: %v", deliverSendToolsOf(t, retryLog))
	}
	if got := deliverSendRequestIDOf(t, calls[len(calls)-1]); got != "pinid" {
		t.Errorf("the retry used request id %q, want the pinned pinid", got)
	}
}

// A notice replaced under a pinned name drops the pin, so the new text is not sent as the old one.
func TestPumpReview776ReplacedPinnedNoticeDropsPin(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "new-body")
	st := pumpTestReadState(t, cfg)
	st.QueueAttempt["parent-1"] = pumpReview776QueuePin{LogicalID: "pinid", Names: []string{"aaaaaaaaaaaaaaaa.txt"}, Body: "old-body"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Error("the pin survived the replaced notice")
	}
	last := deliverSendCallsOf(t, log)
	args, _ := last[len(last)-1]["args"].(map[string]any)
	if message, _ := args["message"].(string); !strings.Contains(message, "new-body") {
		t.Errorf("the batch carried %q, want the replacement body", message)
	}
}

// A cancelled round makes no durable change: no pin, no move and no state write.
func TestPumpReview776CancelledRoundWritesNothing(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	st := pumpTestReadState(t, cfg)
	st.QueueAccepted["parent-1"] = []string{"aaaaaaaaaaaaaaaa.txt"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.StateDir, pumpStateFile)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pumpQueueFlush(ctx, e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a cancelled round rewrote the state:\n%s\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir)); !os.IsNotExist(err) {
		t.Errorf("a cancelled round moved a notice: %v", err)
	}
}
