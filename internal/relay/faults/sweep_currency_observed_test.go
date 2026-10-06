package faults

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-733 (follow-up to CRW-668): the fault sweep asks delivery's own currency question, so its
// decision_reply branch counts the same later receipts delivery.Service.SupersessionReason counts:
// a final, unsuppressed event of the same generation produced by the child or by the relay's own
// observation of an ended turn (daemon_observation), seen later in the order the relay stored the
// receipts. Before this change the sweep counted child receipts only, so an answer the delivery
// path already read as superseded_revision could still read as current in the sweep and keep a
// fault alert. These tests use temporary synthetic stores only.

// obsLedger opens a temporary store with one relationship and returns the sweep's ledger over it.
func obsLedger(t *testing.T) (*Ledger, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, e := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, e = s.Q(ctx).ExecContext(ctx, "INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES('rel','ISSUE','active','parent','host','child','host',1,'[]','[]','stamp','stamp')")
	if e != nil {
		t.Fatal(e)
	}
	return &Ledger{Store: s, Clock: &testClock{now: 100000}}, ctx
}

// obsReceipt seeds one final, unsuppressed event as the relay stores it, seen at the given stamp.
func obsReceipt(t *testing.T, l *Ledger, ctx context.Context, eventID, outcome, producer, seen string, generation int64, receipt string) {
	t.Helper()
	_, e := l.exec(ctx, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES(?,'rel',?,'hash',?,?,'child','turn','completed',?,'final',?,?)", eventID, generation, outcome, producer, receipt, seen, seen)
	if e != nil {
		t.Fatal(e)
	}
}

// obsAnswer seeds the decision_reply that answers the event observedEnd, as RecordDecision stores it
// through queueToChild: the relay records the decision itself, so its producer is relay.
func obsAnswer(t *testing.T, l *Ledger, ctx context.Context, eventID, observedEnd string) {
	t.Helper()
	obsReceipt(t, l, ctx, eventID, "decision_reply", "relay", "2026-01-01T00:00:02Z", 1, `{"answersEvent":"`+observedEnd+`"}`)
}

// obsReason reads the sweep's currency verdict for eventID.
func obsReason(t *testing.T, l *Ledger, ctx context.Context, eventID string) string {
	t.Helper()
	reason, e := f1SupersessionReason(ctx, l, eventID)
	if e != nil {
		t.Fatal(e)
	}
	return reason
}

// c2: an answer to an observed end, followed by a later observation of the same generation, is
// superseded_revision in the sweep's currency, as it already is in delivery's.
func TestObservedCurrency01_ALaterObservationSupersedesTheAnswer(t *testing.T) {
	l, ctx := obsLedger(t)
	obsReceipt(t, l, ctx, "observed-end", "interrupted", "daemon_observation", "2026-01-01T00:00:01Z", 1, "{}")
	obsAnswer(t, l, ctx, "answer", "observed-end")
	obsReceipt(t, l, ctx, "later-end", "failed", "daemon_observation", "2026-01-01T00:00:03Z", 1, "{}")
	if got := obsReason(t, l, ctx, "answer"); got != "superseded_revision" {
		t.Fatalf("the answer after a later observation = %q, want superseded_revision", got)
	}
}

// c2: a later child receipt still makes the answer stale.
func TestObservedCurrency02_ALaterChildReceiptStillSupersedes(t *testing.T) {
	l, ctx := obsLedger(t)
	obsReceipt(t, l, ctx, "observed-end", "interrupted", "daemon_observation", "2026-01-01T00:00:01Z", 1, "{}")
	obsAnswer(t, l, ctx, "answer", "observed-end")
	obsReceipt(t, l, ctx, "child-receipt", "failed", "child", "2026-01-01T00:00:03Z", 1, "{}")
	if got := obsReason(t, l, ctx, "answer"); got != "superseded_revision" {
		t.Fatalf("the answer after a later child receipt = %q, want superseded_revision", got)
	}
}

// c2: an observation seen before the answer was seen does not make it stale.
func TestObservedCurrency03_AnEarlierObservationLeavesTheAnswerCurrent(t *testing.T) {
	l, ctx := obsLedger(t)
	obsReceipt(t, l, ctx, "earlier-end", "failed", "daemon_observation", "2026-01-01T00:00:01Z", 1, "{}")
	obsReceipt(t, l, ctx, "observed-end", "interrupted", "daemon_observation", "2026-01-01T00:00:02Z", 1, "{}")
	obsAnswer(t, l, ctx, "answer", "observed-end")
	if got := obsReason(t, l, ctx, "answer"); got != "" {
		t.Fatalf("an earlier observation made the answer %q, want it current", got)
	}
}

// c2: an observation of another generation does not make the answer stale.
func TestObservedCurrency04_AnotherGenerationsObservationLeavesTheAnswerCurrent(t *testing.T) {
	l, ctx := obsLedger(t)
	obsReceipt(t, l, ctx, "observed-end", "interrupted", "daemon_observation", "2026-01-01T00:00:01Z", 1, "{}")
	obsAnswer(t, l, ctx, "answer", "observed-end")
	obsReceipt(t, l, ctx, "other-generation", "failed", "daemon_observation", "2026-01-01T00:00:03Z", 2, "{}")
	if got := obsReason(t, l, ctx, "answer"); got != "" {
		t.Fatalf("another generation's observation made the answer %q, want it current", got)
	}
}
