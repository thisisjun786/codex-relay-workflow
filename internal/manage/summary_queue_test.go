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

// summaryTestQueueNotice is `crw manage send-parent --queue --logical-id summary-<id>` as the run calls it, answered as the command
// answers: the exit status and the report.
func summaryTestQueueNotice(t *testing.T, id, text string) (int, map[string]any) {
	t.Helper()
	code, stdout := sendParentRunCommand(t, "--thread", "orch-thread", "--message-file", sendParentMessageFile(t, text), "--logical-id", "summary-"+id, "--queue")
	return code, sendParentReply(t, stdout)
}

// summaryTestSends counts the messages the fake bridges carried that contain text, over every log given.
func summaryTestSends(t *testing.T, text string, logs ...string) int {
	t.Helper()
	n := 0
	for _, log := range logs {
		for _, call := range deliverSendCallsOf(t, log) {
			if call["tool"] != deliverToolSend && call["tool"] != deliverToolSteer {
				continue
			}
			if msg, _ := call["args"].(map[string]any)["message"].(string); strings.Contains(msg, text) {
				n++
			}
		}
	}
	return n
}

func summaryTestFlush(t *testing.T, e *Env, cfg *Config) {
	t.Helper()
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
}

func summaryTestQueued(t *testing.T, cfg *Config) []string {
	t.Helper()
	return pumpQueueTestNames(t, cfg, "orch-thread")
}

func summaryTestSettlement(t *testing.T, cfg *Config, id string) (summarySettlement, bool) {
	t.Helper()
	return summaryReadSettlement(cfg, "orch-thread", "summary-"+id)
}

// The notice that a summary entry is owed is delivered exactly once by the real send-parent and the real pump, whenever the run dies
// and asks again: (1) after the queue registration and before it recorded the settlement, (2) after the pump flushed it and before the
// settlement was recorded, and (3) after the pump pinned a batch around it that other notices have since changed. The parent queue's
// other notices keep the path they had.
func TestSummaryNoticeQueueReplayDeliveredOnce(t *testing.T) {
	const text = "summary entry sum-1 is owed"
	t.Run("registered_then_asked_again", func(t *testing.T) {
		now := pumpTestNow
		e := pumpTestEnv(t, &now)
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		})
		cfg := pumpTestConfig(t, bridge)
		sendParentWithConfig(t, cfg)
		code, first := summaryTestQueueNotice(t, "sum-1", text)
		if code != sendParentExitAccepted || first["class"] != sendParentClassQueued || first["state"] != summaryStatePending {
			t.Fatalf("first: %d %v", code, first)
		}
		// the run died before it wrote its settlement, and asks again
		code, again := summaryTestQueueNotice(t, "sum-1", text)
		if code != sendParentExitAccepted || again["class"] != sendParentClassQueued || again["state"] != summaryStatePending {
			t.Fatalf("again: %d %v", code, again)
		}
		if names := summaryTestQueued(t, cfg); len(names) != 1 || names[0] != "summary-sum-1.txt" {
			t.Fatalf("the queue holds %v", names)
		}
		old := now.Add(-2 * time.Hour)
		if err := os.Chtimes(filepath.Join(cfg.StateDir, pumpQueueDir, "orch-thread", "summary-sum-1.txt"), old, old); err != nil {
			t.Fatal(err)
		}
		summaryTestFlush(t, e, cfg)
		if n := summaryTestSends(t, text, log); n != 1 {
			t.Fatalf("the notice was sent %d times", n)
		}
		s, ok := summaryTestSettlement(t, cfg, "sum-1")
		if !ok || s.Result != summaryResultAccepted || s.SummaryID != "sum-1" || s.RequestID == "" {
			t.Errorf("settlement %+v %v", s, ok)
		}
		summaryTestFlush(t, e, cfg)
		if n := summaryTestSends(t, text, log); n != 1 {
			t.Fatalf("a flush after delivery sent it again: %d", n)
		}
	})

	t.Run("flushed_before_the_settlement", func(t *testing.T) {
		now := pumpTestNow
		e := pumpTestEnv(t, &now)
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		})
		cfg := pumpTestConfig(t, bridge)
		sendParentWithConfig(t, cfg)
		summaryTestQueueNotice(t, "sum-1", text)
		old := now.Add(-2 * time.Hour)
		if err := os.Chtimes(filepath.Join(cfg.StateDir, pumpQueueDir, "orch-thread", "summary-sum-1.txt"), old, old); err != nil {
			t.Fatal(err)
		}
		summaryTestFlush(t, e, cfg)
		// the run resumes with no settlement of its own and registers the notice again: the pump has delivered it
		code, again := summaryTestQueueNotice(t, "sum-1", text)
		if code != sendParentExitAccepted || again["class"] != deliverClassAccepted || again["state"] != summaryStateSent || again["requestId"] == "" {
			t.Fatalf("again: %d %v", code, again)
		}
		if names := summaryTestQueued(t, cfg); len(names) != 0 {
			t.Fatalf("the notice was queued again: %v", names)
		}
		summaryTestFlush(t, e, cfg)
		if n := summaryTestSends(t, text, log); n != 1 {
			t.Fatalf("the notice was sent %d times", n)
		}
	})

	t.Run("batch_changed_while_pinned", func(t *testing.T) {
		now := pumpTestNow
		e := pumpTestEnv(t, &now)
		unknown, unknownLog := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
			{"payload": map[string]any{"status": "outcome_unknown"}},
		})
		cfg := pumpTestConfig(t, unknown)
		sendParentWithConfig(t, cfg)
		summaryTestQueueNotice(t, "sum-1", text)
		summaryTestFlush(t, e, cfg)
		if st := pumpTestReadState(t, cfg); len(st.QueueAttempt["orch-thread"].Names) != 1 {
			t.Fatalf("the batch was not pinned: %+v", st.QueueAttempt)
		}
		// another notice joins the queue, and the run asks again for the one that is in the pinned batch
		sendParentRunCommand(t, "--thread", "orch-thread", "--message-file", sendParentMessageFile(t, "an escalation"), "--logical-id", "esc-1", "--queue")
		code, again := summaryTestQueueNotice(t, "sum-1", text)
		if code != sendParentExitAccepted || again["class"] != sendParentClassQueued || again["state"] != summaryStateInFlight {
			t.Fatalf("again: %d %v", code, again)
		}
		if names := summaryTestQueued(t, cfg); len(names) != 2 {
			t.Fatalf("the queue holds %v", names)
		}
		// the bridge says the pinned batch was accepted; the rounds that follow deliver the other notice in a batch of its own
		accepted, acceptedLog := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
		})
		cfg.Bridge.Binary = accepted
		active, activeLog := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-2"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
		})
		summaryTestFlush(t, e, cfg)
		cfg.Bridge.Binary = active
		summaryTestFlush(t, e, cfg)
		summaryTestFlush(t, e, cfg)
		if n := summaryTestSends(t, text, unknownLog, acceptedLog, activeLog); n != 1 {
			t.Fatalf("the notice reached the bridge %d times", n)
		}
		if n := summaryTestSends(t, "an escalation", unknownLog, acceptedLog, activeLog); n != 1 {
			t.Fatalf("the escalation reached the bridge %d times", n)
		}
		if names := summaryTestQueued(t, cfg); len(names) != 0 {
			t.Errorf("queued: %v", names)
		}
		if s, ok := summaryTestSettlement(t, cfg, "sum-1"); !ok || s.Result != summaryResultAccepted {
			t.Errorf("settlement %+v %v", s, ok)
		}
		if _, ok := summaryReadSettlement(cfg, "orch-thread", "esc-1"); ok {
			t.Error("a notice that is no summary left a settlement")
		}
	})
}

// A summary notice sent/ holds already is left out of a batch even when the queue holds a copy (the producer wrote it again after the
// pump moved it): the copy is dropped, not sent.
func TestPumpQueueDropsAQueuedCopyOfADeliveredSummaryNotice(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "t"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, "orch-thread")
	if err := os.MkdirAll(filepath.Join(dir, pumpSentDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pumpSentDir, "summary-sum-1.txt"), []byte("summary entry sum-1 is owed"), 0o600); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestNotice(t, cfg, "orch-thread", "summary-sum-1.txt", "summary entry sum-1 is owed")
	pumpQueueTestNotice(t, cfg, "orch-thread", "zzzzzzzzzzzzzzzz.txt", "another notice")
	summaryTestFlush(t, e, cfg)
	if n := summaryTestSends(t, "summary entry sum-1", log); n != 0 {
		t.Errorf("the delivered notice was sent again: %d", n)
	}
	if n := summaryTestSends(t, "another notice", log); n != 1 {
		t.Errorf("the other notice was sent %d times", n)
	}
	if names := summaryTestQueued(t, cfg); len(names) != 0 {
		t.Errorf("queued: %v", names)
	}
}

// The settlement is JSON a reader can rely on.
func TestSummarySettlementShape(t *testing.T) {
	cfg := pumpTestConfig(t, "/nonexistent")
	pumpSummarySettle(cfg, "orch-thread", "summary-sum-9.txt", summaryResultRefused, "batch-1")
	raw, err := os.ReadFile(summarySettledPath(cfg, "orch-thread", "summary-sum-9"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]string
	if err := json.Unmarshal(raw, &s); err != nil || s["summary_id"] != "sum-9" || s["result"] != "refused" || s["request_id"] != "batch-1" || s["thread"] != "orch-thread" {
		t.Errorf("settlement %s %v", raw, err)
	}
	pumpSummarySettle(cfg, "orch-thread", "esc-1.txt", summaryResultAccepted, "batch-2")
	if _, err := os.Stat(summarySettledPath(cfg, "orch-thread", "esc-1")); err == nil {
		t.Error("a notice that is no summary left a settlement")
	}
}
