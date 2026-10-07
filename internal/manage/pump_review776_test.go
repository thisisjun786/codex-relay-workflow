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
// producer replaced with different content is put back in the queue rather than quarantined, and an
// earlier quarantine is never disturbed.
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
	destination := pumpReview776QueueOversizeName(oversizeDir, "aaaaaaaaaaaaaaaa.txt")
	moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", destination, pumpReview776TestDigest("old text"))
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatalf("a replaced notice was quarantined")
	}
	if raw, err := os.ReadFile(source); err != nil || string(raw) != "new text" {
		t.Errorf("the replacement was disturbed: %q %v", raw, err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Errorf("the replacement reached oversize/: %v", err)
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
	first := pumpReview776QueueOversizeName(oversizeDir, "aaaaaaaaaaaaaaaa.txt")
	moved, err := pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", first, pumpReview776TestDigest("first"))
	if err != nil || !moved {
		t.Fatalf("the first move failed: moved=%v err=%v", moved, err)
	}
	if raw, err := os.ReadFile(first); err != nil || string(raw) != "first" {
		t.Errorf("the quarantined notice is wrong: %q %v", raw, err)
	}
	if err := os.WriteFile(source, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := pumpReview776QueueOversizeName(oversizeDir, "aaaaaaaaaaaaaaaa.txt")
	if second == first {
		t.Fatalf("the second destination reused the first")
	}
	moved, err = pumpReview776QueueMoveVerified(dir, "aaaaaaaaaaaaaaaa.txt", second, pumpReview776TestDigest("second"))
	if err != nil || !moved {
		t.Fatalf("the second move failed: moved=%v err=%v", moved, err)
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
