package manage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// pumpReview776TestDigest is the digest of one notice's trimmed body, the value the pin stores per
// member. The test computes it itself so the file stays self-contained.
func pumpReview776TestDigest(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return fmt.Sprintf("%x", sum[:])
}

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
		if os.IsNotExist(err) {
			// A round that pinned nothing wrote no state file at all.
			return nil, false
		}
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

// A pinned notice that is gone never clears the pin on its own: the attempt may already have gone, so the round still reconciles the pinned id and body.
func TestPumpReview776GonePinnedNoticeKeepsPin(t *testing.T) {
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
	// A gone pinned notice never clears the pin on its own: the attempt may already have gone, so the
	// round still reconciles the pinned id and body, and only the notices that vanished are left out.
	// The empty scenario answers nothing readable, which leaves the attempt unknown and keeps the pin.
	if tools := deliverSendToolsOf(t, log); len(tools) == 0 {
		t.Errorf("a gone pinned notice was not reconciled at all")
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); !ok {
		t.Errorf("a gone pinned notice cleared the pin before the reconciliation answered")
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
	oversizeEntries, err := os.ReadDir(filepath.Join(cfg2.StateDir, pumpQueueDir, "parent-1", pumpReview776OversizeDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(oversizeEntries) != 1 || !strings.HasPrefix(oversizeEntries[0].Name(), "aaaaaaaaaaaaaaaa.") {
		t.Errorf("the oversize notice was not moved aside under a stamped name: %v", oversizeEntries)
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

// An oversize/ directory that is a symlink is refused, so the notice is not moved out of the
// queue through it.
func TestPumpReview776OversizeSymlinkIsRefused(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	threadDir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(threadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(threadDir, pumpReview776OversizeDir)); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	huge := strings.Repeat("y", 130000)
	if err := os.WriteFile(filepath.Join(threadDir, "aaaaaaaaaaaaaaaa.txt"), []byte(huge), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(outside); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Errorf("the notice was moved through the symlink: %v", entries)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 {
		t.Errorf("the notice left the queue: %v", names)
	}
}

// A notice that is not valid UTF-8 is refused before it is pinned, so a persisted pin can never
// disagree with the bytes on disk.
func TestPumpReview776InvalidUTF8NoticeIsNotPinned(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt"), []byte{0xff, 0xfe, 0xfd}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	// The refusal happens before the pin is written, so the flush creates no state file at all: a
	// pin saved with a JSON-mangled body would leave the frozen text disagreeing with the file.
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpStateFile)); !os.IsNotExist(err) {
		t.Errorf("a notice that is not valid UTF-8 was pinned and saved: %v", err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Error("a notice that is not valid UTF-8 was pinned")
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Errorf("a notice that is not valid UTF-8 was sent: %v", deliverSendToolsOf(t, log))
		}
	}
}

// A same-named oversize notice that arrives later must not overwrite the one already in
// oversize/.
func TestPumpReview776OversizeMoveDoesNotOverwrite(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("x", 130000)
	second := strings.Repeat("y", 130001)
	for i, text := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, pumpReview776OversizeDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("oversize/ holds %d notices, want 2 (a later move overwrote the first)", len(entries))
	}
	var all []string
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, pumpReview776OversizeDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, string(raw))
	}
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, first) || !strings.Contains(joined, second) {
		t.Errorf("both quarantined notices were not preserved: %d entries", len(entries))
	}
}

// A pinned batch is reconciled under the pin's own logical id and body even after a member is
// replaced, so an unchanged member is not sent twice.
func TestPumpReview776PinnedBatchReconcilesWhenAMemberChanges(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "body-a")
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "body-b")
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	pin, ok := pumpReview776QueueAttempt(t, cfg, "parent-1")
	if !ok {
		t.Fatal("the unknown batch was not pinned")
	}
	wantID, _ := pin["logical_id"].(string)

	// One member is replaced under the same name before the next round.
	if err := os.WriteFile(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", "aaaaaaaaaaaaaaaa.txt"), []byte("A2"), 0o600); err != nil {
		t.Fatal(err)
	}
	retryBridge, retryLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "not_attempted"}},
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg.Bridge.Binary = retryBridge
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, retryLog)
	reconciled, sends := false, 0
	for _, call := range calls {
		switch call["tool"] {
		case deliverToolOperation:
			if deliverSendRequestIDOf(t, call) == wantID {
				reconciled = true
			}
		case deliverToolSteer, deliverToolSend:
			sends++
			args, _ := call["args"].(map[string]any)
			message, _ := args["message"].(string)
			if !strings.Contains(message, "body-a") || !strings.Contains(message, "body-b") {
				t.Errorf("the resend carried %q, want the pinned body", message)
			}
			if strings.Contains(message, "A2") {
				t.Errorf("the resend carried the replacement: %q", message)
			}
		}
	}
	if !reconciled {
		t.Fatalf("the retry did not reconcile the pinned id %q: %v", wantID, deliverSendToolsOf(t, retryLog))
	}
	if sends > 1 {
		t.Errorf("the retry sent %d times, want at most one", sends)
	}
}

// A notice replaced under a pinned name after the delivery was sent stays queued instead of moving
// to sent/ undelivered.
func TestPumpReview776ReplacedNoticeStaysQueuedAfterAccepted(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The pin holds body1 and the file now carries A2, the text nobody delivered.
	if err := os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt"), []byte("A2"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"queue_attempt": map[string]any{"parent-1": map[string]any{
		"logical_id": "pinid", "names": []string{"aaaaaaaaaaaaaaaa.txt"}, "body": "body1",
		"sha256": map[string]string{"aaaaaaaaaaaaaaaa.txt": pumpReview776TestDigest("body1")}}}}
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
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "aaaaaaaaaaaaaaaa.txt" {
		t.Fatalf("the replacement left the queue: %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); !os.IsNotExist(err) {
		t.Errorf("the replacement was moved to sent/ undelivered: %v", err)
	}
}

// After the logical-id formula changed, a thread with no pin reconciles an unsettled record under
// the old id instead of sending under a new one.
func TestPumpReview776UpgradeReconcilesTheOldLogicalID(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	// The pre-change id: the thread and the names, without the body.
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	newID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("a-body"), CreatedAt: deliverNow(e), State: deliverStatePending}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			if deliverSendRequestIDOf(t, call) == newID {
				t.Errorf("the round sent under the new id %q instead of reconciling the old one", newID)
			}
		}
	}
	reconciled := false
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == oldID {
			reconciled = true
		}
	}
	if !reconciled {
		t.Fatalf("the round did not reconcile the old id %q: %v", oldID, deliverSendToolsOf(t, log))
	}
}

// The quarantining move verifies the notice against the text the round read, so a notice the
// producer replaced with different content stays in the queue rather than being quarantined, and
// nothing is left behind in oversize/.
func TestPumpReview776QuarantineKeepsAReplacedNotice(t *testing.T) {
	dir := t.TempDir()
	oversizeDir := filepath.Join(dir, pumpReview776OversizeDir)
	if err := os.MkdirAll(oversizeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(source, []byte("new text"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The round read "old text"; the file now carries something else, so the move must not happen.
	dest, moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", oversizeDir, pumpReview776TestDigest("old text"))
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatalf("a replaced notice was quarantined")
	}
	if dest != "" {
		t.Errorf("a replaced notice reported a destination: %q", dest)
	}
	if raw, err := os.ReadFile(source); err != nil || string(raw) != "new text" {
		t.Errorf("the replacement was disturbed: %q %v", raw, err)
	}
	if entries, err := os.ReadDir(oversizeDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Errorf("the replacement reached oversize/: %v", entries)
	}
}

// A verified move quarantines the notice it read, never replaces an earlier quarantine, and gives a
// second same-named notice its own destination.
func TestPumpReview776QuarantineMoveIsVerifiedAndUnique(t *testing.T) {
	dir := t.TempDir()
	oversizeDir := filepath.Join(dir, pumpReview776OversizeDir)
	if err := os.MkdirAll(oversizeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(source, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", oversizeDir, pumpReview776TestDigest("first"))
	if err != nil || !moved {
		t.Fatalf("the first move failed: moved=%v err=%v", moved, err)
	}
	if raw, err := os.ReadFile(first); err != nil || string(raw) != "first" {
		t.Errorf("the quarantined notice is wrong: %q %v", raw, err)
	}
	if err := os.WriteFile(source, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", oversizeDir, pumpReview776TestDigest("second"))
	if err != nil || !moved {
		t.Fatalf("the second move failed: moved=%v err=%v", moved, err)
	}
	if second == first {
		t.Fatalf("the second destination reused the first")
	}
	if raw, err := os.ReadFile(first); err != nil || string(raw) != "first" {
		t.Errorf("the earlier quarantine was disturbed: %q %v", raw, err)
	}
	if raw, err := os.ReadFile(second); err != nil || string(raw) != "second" {
		t.Errorf("the second quarantined notice is wrong: %q %v", raw, err)
	}
}

// A pre-change accepted record for a batch that has no pin is completed, not sent again.
func TestPumpReview776LegacyAcceptedRecordIsCompletedNotResent(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("a-body"), CreatedAt: deliverNow(e), State: deliverStateAccepted}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			t.Errorf("an accepted pre-change batch was sent again: %v", deliverSendToolsOf(t, log))
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the accepted notice was not completed: %v", err)
	}
}

// A pre-change unknown record for a batch that has no pin is reconciled under the old id.
func TestPumpReview776LegacyUnknownRecordIsReconciled(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	newID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("a-body"), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, log)
	reconciled := false
	for _, call := range calls {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == oldID {
			reconciled = true
		}
		if (call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend) && deliverSendRequestIDOf(t, call) == newID {
			t.Errorf("the round sent under the new id %q instead of reconciling the old one", newID)
		}
	}
	if !reconciled {
		t.Fatalf("the round did not reconcile the old id %q: %v", oldID, deliverSendToolsOf(t, log))
	}
}

// A dry run with an accepted pin prints only the would-complete line.
func TestPumpReview776DryRunAcceptedPinPrintsOnlyTheLine(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake"})
	notice := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	st := pumpTestReadState(t, cfg)
	st.QueueAttempt["parent-1"] = pumpReview776QueuePin{
		LogicalID: "pinid", Names: []string{"aaaaaaaaaaaaaaaa.txt"}, Body: "a-body",
		SHA256: map[string]string{"aaaaaaaaaaaaaaaa.txt": pumpReview776TestDigest("a-body")}, Accepted: true}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.StateDir, pumpStateFile)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	e.Stdout = &out
	if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), true); code != 0 || err != nil {
		t.Fatalf("the dry round: code=%d err=%v", code, err)
	}
	if strings.Count(out.String(), "queue parent-1") != 1 || !strings.Contains(out.String(), "would complete 1 accepted notices") {
		t.Errorf("the dry run printed %q, want the would-complete line only", out.String())
	}
	if _, err := os.Stat(notice); err != nil {
		t.Errorf("the dry run moved the notice: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the dry run rewrote the state")
	}
}

// An empty notice does not stall the queue: it is quarantined like an oversize one, so the
// notices behind it still go.
func TestPumpReview776EmptyNoticeDoesNotStallTheQueue(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "   ")
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "the real notice")
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "bbbbbbbbbbbbbbbb.txt")); err != nil {
		t.Errorf("the notice behind the empty one did not go: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpReview776OversizeDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the empty notice was not quarantined: %v", entries)
	}
}

// A legacy record for the whole queued set is found even when the size split would deliver only
// the first notice, so the split does not hide an unsettled attempt.
func TestPumpReview776LegacyLookupUsesTheWholeSetBeforeSplitting(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	big := strings.Repeat("x", 46000)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", big)
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", big)
	// The pre-change id hashes the whole queued set, not the split prefix.
	wholeBody := pumpReview776QueueBody([]string{big, big})
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"})
	newID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", big})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256(wholeBody), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, log)
	reconciled := false
	for _, call := range calls {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == oldID {
			reconciled = true
		}
		if (call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend) && deliverSendRequestIDOf(t, call) == newID {
			t.Errorf("the split sent the first notice under the new id instead of reconciling the whole set")
		}
	}
	if !reconciled {
		t.Fatalf("the whole-set legacy record was not reconciled: %v", deliverSendToolsOf(t, log))
	}
}

// A local refusal the delivery core reports before it reads the ledger does not clear a pin: the
// attempt may already have gone, so the pin stays and the next round reconciles it.
func TestPumpReview776LocalRefusalKeepsThePin(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	st := pumpTestReadState(t, cfg)
	st.QueueAttempt["parent-1"] = pumpReview776QueuePin{LogicalID: "pinid", Names: []string{"aaaaaaaaaaaaaaaa.txt"}, Body: "a-body"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	// A bridge policy that is not set is refused before the ledger is read: not evidence about the
	// pinned attempt.
	cfg.Bridge.ExecutionPolicy = ""
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); !ok {
		t.Error("a local refusal cleared the pin without reconciling the attempt")
	}
}

// An accepted pin completes without a resend after a crash between the accepted mark and the
// moves, and a member replaced in between stays queued.
func TestPumpReview776AcceptedPinCompletesAfterACrash(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt"), []byte("sent-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bbbbbbbbbbbbbbbb.txt"), []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"queue_attempt": map[string]any{"parent-1": map[string]any{
		"logical_id": "pinid", "names": []string{"aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"},
		"body": "sent-body", "accepted": true,
		"sha256": map[string]string{
			"aaaaaaaaaaaaaaaa.txt": pumpReview776TestDigest("sent-body"),
			"bbbbbbbbbbbbbbbb.txt": pumpReview776TestDigest("original"),
		}}}}
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
	if tools := deliverSendToolsOf(t, log); len(tools) != 0 {
		t.Errorf("an accepted pin started a bridge process: %v", tools)
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the delivered notice was not completed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir, "bbbbbbbbbbbbbbbb.txt")); !os.IsNotExist(err) {
		t.Errorf("a replaced member was moved to sent/: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "bbbbbbbbbbbbbbbb.txt" {
		t.Errorf("the replaced member did not stay queued: %v", names)
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); ok {
		t.Error("the accepted pin was not cleared after the moves")
	}
}

// A notice whose producer-chosen logical id starts with the aside directory's name is an ordinary
// notice: the aside area is a directory the producer cannot create, so nothing it writes is ever
// mistaken for a move's aside and deleted.
func TestPumpReview776ProducerNameLikeTheAsideDirIsDelivered(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	// The producer wrote a.txt and then, under --logical-id .reclaim-a, a second notice whose name
	// merely starts the way a move's aside used to be named. Both are ordinary notices.
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "first body")
	pumpQueueTestNotice(t, cfg, "parent-1", ".reclaim-aaaaaaaaaaaaaaaa.txt", "second body")
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	delivered := map[string]bool{}
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			args, _ := call["args"].(map[string]any)
			message, _ := args["message"].(string)
			for _, body := range []string{"first body", "second body"} {
				if strings.Contains(message, body) {
					delivered[body] = true
				}
			}
		}
	}
	if !delivered["first body"] || !delivered["second body"] {
		t.Errorf("a notice was lost to the aside recovery: delivered=%v tools=%v", delivered, deliverSendToolsOf(t, log))
	}
}

// A dry run never touches an aside a previous move left behind: the whole round moves no file.
func TestPumpReview776DryRunLeavesAnAsideInPlace(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake"})
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	asideDir := filepath.Join(dir, pumpReview776AsideDir)
	if err := os.MkdirAll(asideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asideDir, "aaaaaaaaaaaaaaaa.txt"), []byte("a-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A notice whose name begins the way an aside does is an ordinary queued notice, and a dry run
	// must leave it where it is too.
	if err := os.WriteFile(filepath.Join(dir, ".reclaim-aaaaaaaaaaaaaaaa.txt"), []byte("b-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := pumpReview776Tree(t, dir)
	if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), true); code != 0 || err != nil {
		t.Fatalf("the dry round: code=%d err=%v", code, err)
	}
	if after := pumpReview776Tree(t, dir); after != before {
		t.Errorf("a dry run changed the queue tree")
	}
}

// The pre-pin membership completion never replaces an entry already under sent/ and never deletes a
// notice the producer wrote.
func TestPumpReview776LegacyMembershipCompletionNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt"), []byte("queued"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An earlier completion already holds this name under sent/.
	if err := os.WriteFile(filepath.Join(sent, "aaaaaaaaaaaaaaaa.txt"), []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pumpReview776QueueMoveByName(dir, []string{"aaaaaaaaaaaaaaaa.txt"}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(sent, "aaaaaaaaaaaaaaaa.txt")); err != nil || string(raw) != "earlier" {
		t.Errorf("the earlier completion was disturbed: %q %v", raw, err)
	}
	entries, err := os.ReadDir(sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("sent/ holds %d entries, want the earlier one and the completed notice", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")); !os.IsNotExist(err) {
		t.Errorf("the queue name survived the completion: %v", err)
	}
}

// A pre-change accepted record whose text no longer matches the queue is not completed by name: a
// member the producer replaced was never sent, so it must not be taken to sent/.
func TestPumpReview776LegacyAcceptedWithAChangedBodyIsNotCompleted(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "A2")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("the accepted body"), CreatedAt: deliverNow(e), State: deliverStateAccepted}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); !os.IsNotExist(err) {
		t.Errorf("an undelivered replacement was completed to sent/: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "aaaaaaaaaaaaaaaa.txt" {
		t.Errorf("the replacement left the queue: %v", names)
	}
}

// An unsettled pre-change record whose names are a prefix of today's queue is still found, so an
// attempt that may already have gone is reconciled instead of re-sent under a new id.
func TestPumpReview776LegacyPrefixNameSetIsReconciled(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	// The upgrade left an unsettled attempt for [a.txt] alone; b.txt arrived afterwards.
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "b-body")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	newID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("a-body"), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	reconciled := false
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == oldID {
			reconciled = true
		}
		if (call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend) && deliverSendRequestIDOf(t, call) == newID {
			t.Errorf("the round sent under the new id %q instead of reconciling the prefix attempt", newID)
		}
	}
	if !reconciled {
		t.Fatalf("the prefix attempt %q was not reconciled: %v", oldID, deliverSendToolsOf(t, log))
	}
}

// A legacy pin taken for an unsettled record whose text the ledger no longer matches carries no
// per-member digest, so an accepted reconciliation cannot complete a notice nobody sent.
func TestPumpReview776LegacyPinCarriesNoDigestForAChangedBody(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "A2")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("the old body"), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	pin, ok := pumpReview776QueueAttempt(t, cfg, "parent-1")
	if !ok {
		t.Fatal("the unsettled record was not pinned")
	}
	if legacy, _ := pin["legacy"].(bool); !legacy {
		t.Errorf("the pin is not marked legacy: %v", pin)
	}
	if sha, present := pin["sha256"]; present {
		if m, _ := sha.(map[string]any); len(m) != 0 {
			t.Errorf("a legacy pin carries per-member digests it cannot justify: %v", m)
		}
	}
}

// A move interrupted between the take-aside and the publish is recovered by the next round: the
// notice goes back to the queue name it came from, so an interrupted move never hides a notice from
// the collector.
func TestPumpReview776InterruptedMoveIsRecovered(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(filepath.Join(dir, pumpReview776AsideDir), 0o700); err != nil {
		t.Fatal(err)
	}
	// A previous move died after it took the notice aside, so only the aside name is on disk.
	aside := filepath.Join(dir, pumpReview776AsideDir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(aside, []byte("a-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	delivered := false
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			args, _ := call["args"].(map[string]any)
			if message, _ := args["message"].(string); strings.Contains(message, "a-body") {
				delivered = true
			}
		}
	}
	if !delivered {
		t.Fatalf("the notice an interrupted move set aside was never delivered: %v", deliverSendToolsOf(t, log))
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the recovered notice was not completed: %v", err)
	}
}

// A move interrupted after the publish is not recovered into the queue: the notice is already under
// sent/, and putting it back would deliver it twice.
func TestPumpReview776PublishedMoveIsNotPutBack(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o700); err != nil {
		t.Fatal(err)
	}
	// A previous move published the notice to sent/ and died before it dropped the aside name.
	asideDir := filepath.Join(dir, pumpReview776AsideDir)
	if err := os.MkdirAll(asideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(asideDir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(aside, []byte("a-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(aside, filepath.Join(sent, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			args, _ := call["args"].(map[string]any)
			if message, _ := args["message"].(string); strings.Contains(message, "a-body") {
				t.Errorf("a notice already published to sent/ was delivered again: %v", deliverSendToolsOf(t, log))
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")); !os.IsNotExist(err) {
		t.Errorf("a published notice was put back into the queue: %v", err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Errorf("the aside name survived the recovery: %v", err)
	}
}

// A legacy pin whose pre-change attempt the ledger accepted, but whose text is not recoverable,
// holds the thread: nothing is archived undelivered and nothing is sent again under a new id.
func TestPumpReview776LegacyAcceptedWithUnrecoverableTextHoldsTheQueue(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	// The old attempt is accepted; the notice on disk is a replacement the attempt never carried. The
	// fake scenario counter restarts with each bridge process, so the first step answers both the
	// queue's active-turn probe and the reconciliation's get_operation.
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1",
			"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "A2")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("the old body"), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	pin, ok := pumpReview776QueueAttempt(t, cfg, "parent-1")
	if !ok {
		t.Fatal("the pin was dropped instead of holding the thread")
	}
	if held, _ := pin["held"].(bool); !held {
		t.Errorf("the pin does not hold: %v", pin)
	}
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			t.Errorf("a notice was sent while the attempt's text is unrecoverable: %v", deliverSendToolsOf(t, log))
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); !os.IsNotExist(err) {
		t.Errorf("a notice nobody can show was delivered was completed: %v", err)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "aaaaaaaaaaaaaaaa.txt" {
		t.Errorf("the notice left the queue: %v", names)
	}
}

// The pre-change lookup finds an unsettled record by its target thread, so a notice whose name
// sorts after the old attempt's names does not hide it.
func TestPumpReview776LegacyLookupFindsANonPrefixNameSet(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	cfg := pumpTestConfig(t, bridge)
	// The old attempt carried z.txt; a.txt arrived afterwards and sorts before it.
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	pumpQueueTestNotice(t, cfg, "parent-1", "zzzzzzzzzzzzzzzz.txt", "z-body")
	oldID := pumpBatchIDStrings([]string{"parent-1", "zzzzzzzzzzzzzzzz.txt"})
	newID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", "zzzzzzzzzzzzzzzz.txt"})
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("z-body"), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	reconciled := false
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == oldID {
			reconciled = true
		}
		if (call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend) && deliverSendRequestIDOf(t, call) == newID {
			t.Errorf("the round sent under the new id %q instead of reconciling the old attempt", newID)
		}
	}
	if !reconciled {
		t.Fatalf("the old attempt %q was not reconciled: %v", oldID, deliverSendToolsOf(t, log))
	}
}

// A move never deletes a notice the producer wrote after the round read the file: the queue name is
// taken aside in one atomic rename and only this move's own aside name is ever removed.
func TestPumpReview776MoveNeverDeletesANewerNotice(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dest")
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(source, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The round read "old"; the producer then replaces the name with a newer notice before the move.
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", destDir, pumpReview776TestDigest("old")); err != nil || moved {
		t.Fatalf("a replaced notice moved: moved=%v err=%v", moved, err)
	}
	if raw, err := os.ReadFile(source); err != nil || string(raw) != "new" {
		t.Errorf("the producer's newer notice was disturbed: %q %v", raw, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || names[0] != "aaaaaaaaaaaaaaaa.txt" || names[1] != "dest" {
		t.Errorf("the queue holds %v, want the notice and dest/ only", names)
	}
}

// A move never replaces an entry that is already there: a second same-named notice takes a name of
// its own and the earlier one survives untouched.
func TestPumpReview776MoveNeverReplacesAnExistingEntry(t *testing.T) {
	dir := t.TempDir()
	oversizeDir := filepath.Join(dir, pumpReview776OversizeDir)
	if err := os.MkdirAll(oversizeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An earlier quarantine already holds this name.
	if err := os.WriteFile(filepath.Join(oversizeDir, "aaaaaaaaaaaaaaaa.txt"), []byte("already there"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", oversizeDir, pumpReview776TestDigest("new"))
	if err != nil || !moved {
		t.Fatalf("the move failed: moved=%v err=%v", moved, err)
	}
	if dest == filepath.Join(oversizeDir, "aaaaaaaaaaaaaaaa.txt") {
		t.Fatalf("the move chose the taken name %q", dest)
	}
	if raw, err := os.ReadFile(filepath.Join(oversizeDir, "aaaaaaaaaaaaaaaa.txt")); err != nil || string(raw) != "already there" {
		t.Errorf("the earlier quarantine was disturbed: %q %v", raw, err)
	}
	if raw, err := os.ReadFile(dest); err != nil || string(raw) != "new" {
		t.Errorf("the moved notice is wrong: %q %v", raw, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Errorf("the source name survived a completed move: %v", err)
	}
}

// A move that did not happen leaves the queue exactly as it was: no stray name the collector would
// ignore, and no notice removed.
func TestPumpReview776FailedMoveLeavesNoStrayName(t *testing.T) {
	dir := t.TempDir()
	oversizeDir := filepath.Join(dir, pumpReview776OversizeDir)
	if err := os.MkdirAll(oversizeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt")
	if err := os.WriteFile(source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The round read "verified text"; the file now carries a replacement, so nothing moves.
	if _, moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", oversizeDir, pumpReview776TestDigest("verified text")); err != nil || moved {
		t.Fatalf("a replaced notice moved: moved=%v err=%v", moved, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || names[0] != "aaaaaaaaaaaaaaaa.txt" || names[1] != pumpReview776OversizeDir {
		t.Errorf("the queue holds %v, want the notice and oversize/ only", names)
	}
	if raw, err := os.ReadFile(source); err != nil || string(raw) != "replacement" {
		t.Errorf("the replacement was disturbed: %q %v", raw, err)
	}
}

// An accepted completion whose move cannot be made keeps the pin, so the notices are neither
// dropped nor sent again: the completion is retried instead of being forgotten.
func TestPumpReview776AcceptedCompletionFailureKeepsThePin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test needs a permission failure, which root does not get")
	}
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aaaaaaaaaaaaaaaa.txt"), []byte("sent-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(dir, pumpSentDir)
	if err := os.MkdirAll(sent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sent, 0o700) })
	pin := pumpReview776QueuePin{LogicalID: "pinid", Names: []string{"aaaaaaaaaaaaaaaa.txt"}, Body: "sent-body",
		SHA256: map[string]string{"aaaaaaaaaaaaaaaa.txt": pumpReview776TestDigest("sent-body")}, Accepted: true}
	st := pumpTestReadState(t, cfg)
	st.QueueAttempt["parent-1"] = pin
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	if err := pumpReview776QueueFinishAccepted(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), dir, "parent-1", pin, false); err == nil {
		t.Fatal("a completion whose move failed reported success")
	}
	if _, ok := pumpReview776QueueAttempt(t, cfg, "parent-1"); !ok {
		t.Error("a failed completion dropped the pin, so the notices would be sent again")
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 1 || names[0] != "aaaaaaaaaaaaaaaa.txt" {
		t.Errorf("the notice left the queue on a failed completion: %v", names)
	}
}

// An unsettled pre-change record whose text the ledger does not store is reconciled under the old
// id through the bridge's own receipt, not sent under the new id.
func TestPumpReview776LegacyUnsettledRecordIsReconciledDespiteAChangedBody(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "A2")
	pumpQueueTestNotice(t, cfg, "parent-1", "bbbbbbbbbbbbbbbb.txt", "b-body")
	oldID := pumpBatchIDStrings([]string{"parent-1", "aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"})
	// The pre-change attempt's text is not recoverable: the ledger holds a digest of a body nobody
	// can rebuild from the notices now on disk.
	if err := deliverSave(cfg, deliverRecord{
		LogicalID: oldID, RequestID: oldID, Tool: deliverToolSend, TargetThread: "parent-1",
		MessageSHA256: deliverMessageSHA256("the old body nobody can rebuild"), CreatedAt: deliverNow(e), State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	reconciled := false
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) == oldID {
			reconciled = true
		}
		if call["tool"] == deliverToolSteer || call["tool"] == deliverToolSend {
			t.Errorf("the round sent instead of reconciling the old id: %v", deliverSendToolsOf(t, log))
		}
	}
	if !reconciled {
		t.Fatalf("the old id %q was not reconciled: %v", oldID, deliverSendToolsOf(t, log))
	}
	pin, ok := pumpReview776QueueAttempt(t, cfg, "parent-1")
	if !ok {
		t.Fatal("the unsettled old attempt was not pinned")
	}
	if pin["logical_id"] != oldID {
		t.Errorf("the pin names %v, want the old id %q", pin["logical_id"], oldID)
	}
	if legacy, _ := pin["legacy"].(bool); !legacy {
		t.Errorf("the pin is not marked legacy: %v", pin)
	}
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 2 {
		t.Errorf("a notice left the queue during the reconciliation: %v", names)
	}
}
