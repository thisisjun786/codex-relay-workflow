package manage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deliverTestConfig is a configuration whose outbox lives in a fresh temporary directory, so no
// test reaches a real state directory.
func deliverTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := coreDefaults(&Env{Getenv: os.Getenv})
	cfg.StateDir = t.TempDir()
	return cfg
}

// deliverRecordOf reads the outbox record of one logical message.
func deliverRecordOf(t *testing.T, cfg *Config, logicalID string) deliverRecord {
	t.Helper()
	raw, err := os.ReadFile(deliverOutboxPath(cfg, logicalID))
	if err != nil {
		t.Fatalf("the outbox record of %q: %v", logicalID, err)
	}
	var record deliverRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	return record
}

// The contract inputs of the issue body's table classify as the allowlist says, and the refused
// cases are told apart from the unknown ones. deliverClassify is called directly: this file
// carries no bridge session.
func TestDeliverClassifyTheContractInputs(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		reply deliverReply
		want  string
	}{
		{"accepted send", deliverToolSend, deliverReply{Payload: map[string]any{"status": "accepted", "delivery": "turn_started"}}, deliverClassAccepted},
		{"accepted steer", deliverToolSteer, deliverReply{Payload: map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}}, deliverClassAccepted},
		{"the real ledger shape", deliverToolSend, deliverReply{Payload: map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}}, deliverClassUnknown},
		{"outcome unknown", deliverToolSend, deliverReply{Payload: map[string]any{"status": "outcome_unknown", "delivery": "outcome_unknown"}}, deliverClassUnknown},
		{"outcome unknown with an error", deliverToolSend, deliverReply{Payload: map[string]any{"status": "outcome_unknown", "error": "boom"}}, deliverClassUnknown},
		{"empty response", deliverToolSend, deliverReply{Payload: map[string]any{}}, deliverClassUnknown},
		{"a payload that is not there", deliverToolSend, deliverReply{Text: "not json"}, deliverClassUnknown},
		{"a transport failure", deliverToolSend, deliverReply{Err: errors.New("the bridge closed its output")}, deliverClassUnknown},
		{"not delivered", deliverToolSend, deliverReply{Payload: map[string]any{"delivery": "not_delivered"}}, deliverClassRefused},
		{"rejected", deliverToolSend, deliverReply{Payload: map[string]any{"delivery": "rejected"}}, deliverClassRefused},
		{"refused", deliverToolSend, deliverReply{Payload: map[string]any{"status": "refused"}}, deliverClassRefused},
		{"not attempted", deliverToolSend, deliverReply{Payload: map[string]any{"status": "not_attempted", "retrySafe": true}}, deliverClassRefused},
		{"a tool error the bridge raised", deliverToolSend, deliverReply{IsError: true, Text: "Error executing tool send_message_to_thread: nope"}, deliverClassRefused},
		{"an error text without the flag", deliverToolSend, deliverReply{Text: "Error executing tool send_message_to_thread: nope"}, deliverClassRefused},
		{"a contradictory receipt fails closed", deliverToolSend, deliverReply{Payload: map[string]any{"status": "refused", "delivery": "turn_started"}}, deliverClassRefused},
		{"an accepted delivery of the wrong tool", deliverToolSend, deliverReply{Payload: map[string]any{"delivery": "accepted_not_applied"}}, deliverClassUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deliverClassify(c.tool, c.reply); got != c.want {
				t.Errorf("deliverClassify = %q, want %q (reply %+v)", got, c.want, c.reply)
			}
		})
	}
}

// A record written before the first send carries the logical id as its request id, the message's
// sha256, the pending state and the clock's stamp.
func TestDeliverNewRecordCarriesTheIdentityAndTheDigest(t *testing.T) {
	stamp := time.Date(2026, 10, 6, 7, 30, 0, 0, time.UTC)
	e := &Env{Getenv: os.Getenv, Now: func() time.Time { return stamp }}
	record := deliverNewRecord(e, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if record.LogicalID != "m1" || record.RequestID != "m1" || record.TargetThread != "thread-1" {
		t.Errorf("the identity: %+v", record)
	}
	if record.State != deliverStatePending || record.Received || record.Applied {
		t.Errorf("the state before the send: %+v", record)
	}
	if record.CreatedAt != stamp.Format(time.RFC3339) {
		t.Errorf("created_at = %q, want %q", record.CreatedAt, stamp.Format(time.RFC3339))
	}
	// The digest of the three bytes h, e, l, l, o is the well known sha256 below.
	const helloDigest = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if record.MessageSHA256 != helloDigest {
		t.Errorf("message_sha256 = %q, want %s", record.MessageSHA256, helloDigest)
	}
}

// The record is written through a temporary file, synced and renamed, and reads back equal.
func TestDeliverRecordRoundTripsAtomically(t *testing.T) {
	cfg := deliverTestConfig(t)
	record := deliverRecord{
		LogicalID:     "m1",
		RequestID:     "m1",
		Tool:          deliverToolSend,
		TargetThread:  "thread-1",
		MessageSHA256: "abc",
		CreatedAt:     "2026-10-06T07:30:00Z",
		State:         deliverStateUnknown,
		Received:      true,
		Applied:       false,
		Attempts: []deliverAttempt{
			{At: "2026-10-06T07:30:00Z", Class: deliverClassUnknown, ReceiptExcerpt: "outcome_unknown"},
			{At: "2026-10-06T07:31:00Z", Class: deliverClassAccepted, ReceiptExcerpt: "reconciled with get_operation"},
		},
	}
	if err := deliverSave(cfg, record); err != nil {
		t.Fatalf("deliverSave: %v", err)
	}
	if _, err := os.Stat(deliverOutboxPath(cfg, "m1") + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temporary file survived the rename: %v", err)
	}
	got := deliverRecordOf(t, cfg, "m1")
	if got.LogicalID != record.LogicalID || got.RequestID != record.RequestID || got.Tool != record.Tool ||
		got.TargetThread != record.TargetThread || got.MessageSHA256 != record.MessageSHA256 ||
		got.CreatedAt != record.CreatedAt || got.State != record.State || got.Received != record.Received ||
		got.Applied != record.Applied || len(got.Attempts) != len(record.Attempts) {
		t.Fatalf("the record read back as %+v, want %+v", got, record)
	}
	for i := range record.Attempts {
		if got.Attempts[i] != record.Attempts[i] {
			t.Errorf("attempt %d read back as %+v, want %+v", i, got.Attempts[i], record.Attempts[i])
		}
	}
	if loaded, known, err := deliverLoad(cfg, "m1"); err != nil || !known || loaded.RequestID != "m1" {
		t.Errorf("deliverLoad: %+v %v %v", loaded, known, err)
	}
}

// Every state the record can hold reads back equal, so the state is the one that was written.
func TestDeliverRecordStatesRoundTrip(t *testing.T) {
	cfg := deliverTestConfig(t)
	for _, state := range []string{deliverStatePending, deliverStateAccepted, deliverStateRefused, deliverStateUnknown} {
		t.Run(state, func(t *testing.T) {
			if err := deliverSave(cfg, deliverRecord{LogicalID: "s-" + state, RequestID: "s", State: state}); err != nil {
				t.Fatalf("deliverSave: %v", err)
			}
			if got := deliverRecordOf(t, cfg, "s-"+state); got.State != state {
				t.Errorf("state read back as %q, want %q", got.State, state)
			}
		})
	}
}

// A record that was never written is not an error: it is the first attempt at that message.
func TestDeliverLoadOfAMissingRecord(t *testing.T) {
	cfg := deliverTestConfig(t)
	record, known, err := deliverLoad(cfg, "never-written")
	if err != nil || known || record.RequestID != "" {
		t.Errorf("deliverLoad of a missing record: %+v %v %v", record, known, err)
	}
}

// A logical id that is not one path element is refused, and nothing is written anywhere.
func TestDeliverRefusesAnUnsafeLogicalID(t *testing.T) {
	cfg := deliverTestConfig(t)
	backslash := string(rune(0x5c))
	for _, id := range []string{"", ".", "..", "a/b", "a" + backslash + "b", "../escape", "a/../b"} {
		if err := deliverPathComponent(id, "logical id"); err == nil {
			t.Errorf("the logical id %q was accepted", id)
		}
		if err := deliverSave(cfg, deliverRecord{LogicalID: id, RequestID: "r", State: deliverStatePending}); err == nil {
			t.Errorf("a record was written for the logical id %q", id)
		}
	}
	for _, safe := range []string{"m1", "m1-r1", "0123456789abcdef"} {
		if err := deliverPathComponent(safe, "logical id"); err != nil {
			t.Errorf("the safe logical id %q was refused: %v", safe, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("an unsafe logical id wrote %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.StateDir), "escape.json")); !os.IsNotExist(err) {
		t.Errorf("a record escaped the state directory: %v", err)
	}
}

// deliverWriteAtomic commits through the rename, so the target is never half written and no
// temporary file is left behind.
func TestDeliverWriteAtomicLeavesOnlyTheTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "record.json")
	if err := deliverWriteAtomic(path, []byte("first")); err != nil {
		t.Fatalf("deliverWriteAtomic: %v", err)
	}
	if err := deliverWriteAtomic(path, []byte("second")); err != nil {
		t.Fatalf("the second write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Errorf("the target holds %q %v, want %q", data, err, "second")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "record.json" {
		t.Errorf("the directory holds %v, want only the target", entries)
	}
}

// Only a not_delivered refusal is retryable.
func TestDeliverRetryable(t *testing.T) {
	cases := []struct {
		name  string
		reply deliverReply
		want  bool
	}{
		{"not delivered", deliverReply{Payload: map[string]any{"delivery": "not_delivered"}}, true},
		{"rejected", deliverReply{Payload: map[string]any{"delivery": "rejected"}}, false},
		{"outcome unknown", deliverReply{Payload: map[string]any{"delivery": "outcome_unknown"}}, false},
		{"a transport failure", deliverReply{Err: errors.New("closed")}, false},
	}
	for _, c := range cases {
		if got := deliverRetryable(c.reply); got != c.want {
			t.Errorf("%s: deliverRetryable = %v, want %v", c.name, got, c.want)
		}
	}
}

// The excerpt is the receipt text, or the transport error, cut to the limit.
func TestDeliverExcerpt(t *testing.T) {
	receipt := "status outcome_unknown delivery outcome_unknown"
	if got := deliverExcerpt(deliverReply{Text: receipt}); got != receipt {
		t.Errorf("deliverExcerpt = %q, want %q", got, receipt)
	}
	if got := deliverExcerpt(deliverReply{Err: errors.New("the bridge closed its output")}); got != "the bridge closed its output" {
		t.Errorf("deliverExcerpt of a transport failure = %q", got)
	}
	long := deliverExcerpt(deliverReply{Text: strings.Repeat("x", deliverExcerptLimit+50)})
	if len(long) != deliverExcerptLimit {
		t.Errorf("the excerpt is %d characters, want %d", len(long), deliverExcerptLimit)
	}
}
