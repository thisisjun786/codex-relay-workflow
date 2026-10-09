package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pumpQueueTestLog is the pump's log so far.
func pumpQueueTestLog(t *testing.T, cfg *Config) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpLogFile))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// pumpQueueTestQueuedText is the text a queued notice holds, or "" with false when it is not queued.
func pumpQueueTestQueuedText(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		t.Fatal(err)
	}
	return string(raw), true
}

// A pre-change record over [a b] whose a was written again after it says nothing provable about the
// text b holds now: the old pump read the queue, then could take seconds (the active-turn probe)
// before Deliver stamped the record, and an atomic --queue write of b in between leaves a b older than
// the stamp that the attempt never carried. The record's message digest covers a's old text, which is
// gone, so b's text cannot be checked against it. Such a record never completes b by its modification
// time, and its answer never lets b be sent either: whatever it answers -- went, never went, or nothing
// settled -- the thread holds with a named log line for the operator, so nothing is archived
// undelivered and nothing is sent twice. A record over [b] alone that never went does not lift the
// hold: the record over [a b] may still have carried b's text.
func TestPumpQueuePartlyReplacedLegacyRecordIsNoProofOfTheText(t *testing.T) {
	a, b := pumpOverlapA, pumpOverlapB
	cases := []struct {
		name string
		// ledger is the record's own state; receipt is its get_operation answer.
		ledger, receipt string
		// short adds an unsettled record over [b] that carries b's current text, with this receipt.
		short string
		// held: the pin holds the thread; pinned: a pin is left at all.
		held, pinned bool
		// sent is how often each current text goes.
		sent    int
		logName string
	}{
		{name: "accepted-in-ledger", ledger: deliverStateAccepted, held: true, pinned: true, logName: pumpQueueLegacyUnprovableHold},
		{name: "accepted-by-receipt", ledger: deliverStateUnknown, receipt: pumpOverlapAccepted, held: true, pinned: true, logName: pumpQueueLegacyUnprovableHold},
		{name: "overlap-accepted-with-a-whole-record-that-never-went", ledger: deliverStateUnknown, receipt: pumpOverlapAccepted, short: pumpOverlapNotAttempted, held: true, pinned: true, logName: pumpQueueLegacyUnprovableHold},
		{name: "undetermined", ledger: deliverStateUnknown, receipt: pumpOverlapUnknown, held: true, pinned: true, logName: pumpQueueLegacyUnprovableHold},
		{name: "never-went", ledger: deliverStateUnknown, receipt: pumpOverlapNotAttempted, held: true, pinned: true, logName: pumpQueueLegacyUnprovableHold},
		{name: "overlap-never-went", ledger: deliverStateUnknown, receipt: pumpOverlapNotAttempted, short: pumpOverlapRefused, held: true, pinned: true, logName: pumpQueueLegacyUnprovableHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, receiptsPath, log := pumpOverlapBridge(t)
			cfg := pumpTestConfig(t, bridge)
			thread := "parent-1"
			dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
			stamp := now.Add(-time.Hour)
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, a, "A-new-after-record"), stamp.Add(time.Minute))
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, b, "B-new-before-record"), stamp.Add(-time.Minute))
			longID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{a, b}, []string{"A-old", "B-old"}, stamp, tc.ledger)
			receipts := map[string]string{longID: tc.receipt}
			if tc.short != "" {
				shortID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{b}, []string{"B-new-before-record"}, stamp, deliverStateUnknown)
				receipts[shortID] = tc.short
			}
			pumpOverlapSetReceipts(t, receiptsPath, receipts)
			for round := 0; round < 3; round++ {
				if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
					t.Fatal(err)
				}
			}
			for _, text := range []string{"A-new-after-record", "B-new-before-record"} {
				if got := pumpOverlapSentCount(t, log, text); got != tc.sent {
					t.Errorf("%s was sent %d times, want %d", text, got, tc.sent)
				}
			}
			for _, name := range []string{a, b} {
				_, completed := pumpQueueTestQueuedText(t, filepath.Join(dir, pumpSentDir), name)
				_, queued := pumpQueueTestQueuedText(t, dir, name)
				if tc.sent == 0 && (completed || !queued) {
					t.Errorf("%s was never sent and is completed=%v queued=%v; it must stay queued", name, completed, queued)
				}
				if tc.sent == 1 && (!completed || queued) {
					t.Errorf("%s was sent and is completed=%v queued=%v", name, completed, queued)
				}
			}
			pin, pinned := pumpReview776QueueAttempt(t, cfg, thread)
			if pinned != tc.pinned {
				t.Errorf("pin present=%v, want %v (%v)", pinned, tc.pinned, pin)
			}
			if held, _ := pin["held"].(bool); held != tc.held {
				t.Errorf("pin held=%v, want %v (%v)", held, tc.held, pin)
			}
			if tc.logName != "" && !strings.Contains(pumpQueueTestLog(t, cfg), tc.logName) {
				t.Errorf("no %s line in the log:\n%s", tc.logName, pumpQueueTestLog(t, cfg))
			}
		})
	}
}

// The same race through a real Deliver: the old pump reads A-old and B-old, B is replaced before the
// delivery core stamps its record, and A is replaced after it. The accepted ledger record is the core's
// own, the replacement of B is a normal atomic write a second before the stamp, and B-new was never
// sent: it must not reach sent/ unsent.
func TestPumpQueueRealAcceptedSnapshotBeforeTheStampIsNoProof(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	a, b := pumpOverlapA, pumpOverlapB
	aPath := pumpQueueTestNotice(t, cfg, thread, a, "A-old")
	bPath := pumpQueueTestNotice(t, cfg, thread, b, "B-old")
	pumpQueueTestSetTime(t, aPath, stamp.Add(-10*time.Second))
	pumpQueueTestSetTime(t, bPath, stamp.Add(-10*time.Second))
	names := []string{a, b}
	dir := filepath.Dir(aPath)
	texts, _, err := pumpReview776QueueReadNotices(dir, names)
	if err != nil {
		t.Fatal(err)
	}
	replaced := false
	e.Now = func() time.Time {
		if !replaced {
			// The producer's atomic write lands between the old pump's read and the core's stamp.
			replaced = true
			if err := os.WriteFile(bPath+".tmp", []byte("B-new-never-sent"), 0o600); err != nil {
				t.Fatal(err)
			}
			pumpQueueTestSetTime(t, bPath+".tmp", stamp.Add(-2*time.Second))
			if err := os.Rename(bPath+".tmp", bPath); err != nil {
				t.Fatal(err)
			}
		}
		return stamp
	}
	oldID := pumpQueueLegacyBatchID(thread, names)
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: oldID, Thread: thread, Text: pumpReview776QueueBody(texts), Role: "parent", Settings: cfg.Settings.Parent})
	if err != nil || out.Class != deliverClassAccepted {
		t.Fatalf("the pre-change delivery answered %v (%v)", out, err)
	}
	e.Now = func() time.Time { return now }
	if err := os.WriteFile(aPath+".tmp", []byte("A-new-after-record"), 0o600); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestSetTime(t, aPath+".tmp", stamp.Add(2*time.Second))
	if err := os.Rename(aPath+".tmp", aPath); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
	}
	sent := pumpOverlapSentCount(t, log, "B-new-never-sent")
	if raw, err := os.ReadFile(filepath.Join(dir, pumpSentDir, b)); err == nil && sent == 0 {
		t.Errorf("B-new reached sent/ without a send: %q", raw)
	}
	if sent > 1 {
		t.Errorf("B-new was sent %d times", sent)
	}
	if a, b := pumpOverlapSentCount(t, log, "A-old"), pumpOverlapSentCount(t, log, "B-old"); a != 1 || b != 1 {
		t.Errorf("A-old went %d times and B-old %d times, want only the pre-change delivery of each", a, b)
	}
}

// pumpQueueReplacingBridge wraps a fake bridge so that its first start replaces the named notices
// with an atomic rename, the way the producer's --queue write does: the replacement lands after the
// round read the queue and while it reads the pre-change records' receipts.
func pumpQueueReplacingBridge(t *testing.T, bridge, dir string, replacements map[string]string) string {
	t.Helper()
	wrapper := filepath.Join(filepath.Dir(bridge), "replacing-bridge")
	marker := wrapper + ".done"
	script := "#!/bin/sh\nif [ ! -e " + coreShellQuote(marker) + " ]; then\n"
	for name, text := range replacements {
		path := filepath.Join(dir, name)
		script += "printf %s " + coreShellQuote(text) + " > " + coreShellQuote(path+".tmp") + "\nmv " + coreShellQuote(path+".tmp") + " " + coreShellQuote(path) + "\n"
	}
	script += "touch " + coreShellQuote(marker) + "\nfi\nexec " + coreShellQuote(bridge) + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

// Two overlapping pre-change records over [a b] and [a] carried the texts the round read. While the
// round reads their receipts the producer replaces both notices. The round must not send the texts it
// read under a new id whatever the receipts say: they may be the texts the records delivered, and the
// queue on disk is no longer what the round read. The next round reads the new notices, which no record
// carried, and sends each exactly once.
func TestPumpQueueOverlapSnapshotReplacedDuringTheReceiptReadSendsNothingStale(t *testing.T) {
	kinds := []string{pumpOverlapAccepted, pumpOverlapNotAttempted, pumpOverlapUnknown, pumpOverlapUnreachable}
	for _, long := range kinds {
		for _, short := range kinds {
			t.Run(long+"+"+short, func(t *testing.T) {
				now := pumpTestNow
				e := pumpTestEnv(t, &now)
				bridge, receiptsPath, log := pumpOverlapBridge(t)
				cfg := pumpTestConfig(t, bridge)
				thread := "parent-1"
				dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
				stamp := now.Add(-time.Hour)
				names, texts := []string{pumpOverlapA, pumpOverlapB}, []string{"A-already-delivered", "B-already-delivered"}
				for i, name := range names {
					pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
				}
				longID := pumpQueueTestLegacyRecord(t, cfg, thread, names, texts, stamp, deliverStateUnknown)
				shortID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:1], texts[:1], stamp, deliverStateUnknown)
				pumpOverlapSetReceipts(t, receiptsPath, map[string]string{longID: long, shortID: short})
				replacements := map[string]string{pumpOverlapA: "A-replaced", pumpOverlapB: "B-replaced"}
				cfg.Bridge.Binary = pumpQueueReplacingBridge(t, bridge, dir, replacements)
				if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(cfg.Bridge.Binary + ".done"); err != nil {
					t.Fatalf("the replacement did not run: %v", err)
				}
				for _, text := range texts {
					if got := pumpOverlapSentCount(t, log, text); got != 0 {
						t.Errorf("the text the round read, %q, was sent %d times after the queue changed under it", text, got)
					}
				}
				// The new notices were written after both records.
				for _, name := range names {
					pumpQueueTestSetTime(t, filepath.Join(dir, name), stamp.Add(time.Minute))
				}
				for round := 0; round < 3; round++ {
					if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
						t.Fatal(err)
					}
				}
				for _, text := range texts {
					if got := pumpOverlapSentCount(t, log, text); got != 0 {
						t.Errorf("%q was sent %d times", text, got)
					}
				}
				for _, name := range names {
					if got := pumpOverlapSentCount(t, log, replacements[name]); got != 1 {
						t.Errorf("%q was sent %d times, want once", replacements[name], got)
					}
					if raw, ok := pumpQueueTestQueuedText(t, filepath.Join(dir, pumpSentDir), name); !ok || raw != replacements[name] {
						t.Errorf("sent/%s holds %q (%v), want %q", name, raw, ok, replacements[name])
					}
				}
				if left := pumpQueueTestNames(t, cfg, thread); len(left) != 0 {
					t.Errorf("notices left queued: %v", left)
				}
				if pin, pinned := pumpReview776QueueAttempt(t, cfg, thread); pinned {
					t.Errorf("a pin is left: %v", pin)
				}
			})
		}
	}
}

// pumpQueueCancelOnSent is a real cancellable context whose cancellation fires the moment the first
// notice is published under sent/: Done closes and Err reports it from then on, as a SIGINT that lands
// in the middle of an accepted batch's moves.
type pumpQueueCancelOnSent struct {
	context.Context
	cancel context.CancelFunc
	sent   string
}

func (c *pumpQueueCancelOnSent) Err() error {
	if entries, err := os.ReadDir(c.sent); err == nil && len(entries) > 0 {
		c.cancel()
	}
	return c.Context.Err()
}

// A round cancelled after the first move of an accepted batch moves nothing more and keeps the
// accepted pin, so the next round completes the rest from it without a send.
func TestPumpQueueAcceptedCompletionStopsAtCancellation(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
	thread := "parent-1"
	var names, texts []string
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("%016d.txt", i)
		pumpQueueTestNotice(t, cfg, thread, name, "delivered "+name)
		names, texts = append(names, name), append(texts, "delivered "+name)
	}
	pumpQueueTestPin(t, cfg, thread, "acceptedid", names, texts)
	st := pumpTestReadStatePtr(t, cfg)
	pin := st.QueueAttempt[thread]
	pin.Accepted = true
	st.QueueAttempt[thread] = pin
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	sent := filepath.Join(dir, pumpSentDir)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &pumpQueueCancelOnSent{Context: parent, cancel: cancel, sent: sent}
	if err := pumpQueueFlush(ctx, e, cfg, st, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if parent.Err() == nil {
		t.Fatal("the round was never cancelled")
	}
	entries, err := os.ReadDir(sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the cancelled round left %d notices in sent/, want the one moved before the cancellation", len(entries))
	}
	disk, pinned := pumpReview776QueueAttempt(t, cfg, thread)
	if !pinned || disk["accepted"] != true {
		t.Fatalf("the cancelled round did not keep the accepted pin: %v", disk)
	}
	if _, inMemory := st.QueueAttempt[thread]; !inMemory {
		t.Errorf("the cancelled round dropped the pin from memory but not from disk")
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(sent); err != nil || len(entries) != len(names) {
		t.Errorf("the next round left sent/ with %d notices (%v), want %d", len(entries), err, len(names))
	}
	if left := pumpQueueTestNames(t, cfg, thread); len(left) != 0 {
		t.Errorf("notices left queued: %v", left)
	}
	if pin, pinned := pumpReview776QueueAttempt(t, cfg, thread); pinned {
		t.Errorf("the pin survived the completing round: %v", pin)
	}
}
