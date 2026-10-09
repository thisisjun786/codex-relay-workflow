package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// R1/R2: a pre-change record that cannot prove its text holds the thread whatever its answer, and the
// ledger's own refusal is such an answer: a refused record over [a,b] whose a was written again after it
// (and whose digest is not the text on disk) holds, with a stored held pin, instead of letting the new a
// and the current b go out.
func TestPumpQueueUnprovableRefusedLegacyRecordHolds(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapA, "A-new"), stamp.Add(time.Minute))
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapB, "B-current"), stamp.Add(-time.Minute))
	oldID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapA, pumpOverlapB}, []string{"A-old", "B-old"}, stamp, deliverStateRefused)
	pumpLegacyHoldFlush(t, e, cfg, 2)
	if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
		t.Errorf("R2: the unprovable refused record %s let a batch go: %q", oldID, messages)
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); !ok || pin["held"] != true {
		t.Errorf("R2: no held pin for the unprovable refused record: %v", pin)
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyUnprovableHold) || !strings.Contains(logText, oldID) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyUnprovableHold, oldID, logText)
	}
}

// A provable refused record sent nothing, so its notices are sent once (R1), as before.
func TestPumpQueueProvableRefusedLegacyRecordSendsOnce(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	names, texts := []string{pumpOverlapA, pumpOverlapB}, []string{"A-current", "B-current"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	pumpQueueTestLegacyRecord(t, cfg, thread, names, texts, stamp, deliverStateRefused)
	pumpLegacyHoldFlush(t, e, cfg, 3)
	for _, text := range texts {
		if n := pumpOverlapSentCount(t, log, text); n != 1 {
			t.Errorf("R1: %s sent %d times, want once: %q", text, n, pumpQueueTestSentMessages(t, log))
		}
	}
}

// A queue too large for the search holds on a record that may have carried a queued notice even when
// that record was refused: its members are unknown, so it cannot prove its text (R2).
func TestPumpQueueLegacyScanLimitHoldsOnARefusedRecord(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	var names []string
	for i := 0; i <= pumpQueueLegacyScanLimit; i++ {
		name := fmt.Sprintf("%016x.txt", i+1)
		names = append(names, name)
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, fmt.Sprintf("N%02d", i)), stamp.Add(-time.Minute))
	}
	pumpQueueTestLegacyRecord(t, cfg, thread, names[:2], []string{"N00", "N01"}, stamp, deliverStateRefused)
	pumpLegacyHoldFlush(t, e, cfg, 2)
	if sent := pumpQueueTestSentMessages(t, log); len(sent) != 0 {
		t.Errorf("a batch was sent while a refused pre-change record of unknown members may cover it: %q", sent)
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyScanLimitHold) {
		t.Errorf("no %s line:\n%s", pumpQueueLegacyScanLimitHold, logText)
	}
}

// An earlier build adopted a pre-change record whose text matched as an ordinary pin under the old id
// (no Legacy mark, no overlap). The search must still run before that pin is reconciled, so a shorter
// accepted record over [a] keeps a from being sent again; b, which the long record never sent, goes once.
func TestPumpQueueEarlierOldIDPinKeepsAcceptedLegacyEvidence(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receipts, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapA, pumpOverlapB}
	texts := []string{"A-already-delivered", "B-never-delivered"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	longID := pumpQueueTestLegacyRecord(t, cfg, thread, names, texts, stamp, deliverStateUnknown)
	shortID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:1], texts[:1], stamp, deliverStateAccepted)
	pumpOverlapSetReceipts(t, receipts, map[string]string{longID: pumpOverlapNotAttempted, shortID: pumpOverlapAccepted})
	pumpQueueTestPin(t, cfg, thread, longID, names, texts)
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, texts[0]); n != 0 {
		t.Errorf("I1: the old-id pin %s hid the accepted record %s and a went %d more times: %q", longID, shortID, n, pumpQueueTestSentMessages(t, log))
	}
	if n := pumpOverlapSentCount(t, log, texts[1]); n != 1 {
		t.Errorf("I3: b sent %d times, want once: %q", n, pumpQueueTestSentMessages(t, log))
	}
	if left, _ := pumpQueueSortedNames(dir); len(left) != 0 {
		t.Errorf("notices left queued: %v", left)
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); ok {
		t.Errorf("a pin is left: %v", pin)
	}
}

// An earlier size-split build left a pin over the prefix [a,b] of [a,b,z] whose delivery the ledger
// accepted. Completing it moves b, after which the pre-change record over [b,z] -- accepted, z
// unchanged, b written again after it -- is no longer a set of the queue. The search runs before the
// completion, so the record is found while b is still queued: it cannot prove its text, the thread holds
// with its named line, a and b are completed under the hold (the pin's own accepted record proves them),
// and z is not sent again.
func TestPumpQueueAcceptedPinCannotHideALegacyMember(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	aText := "A-new-" + strings.Repeat("a", 45000)
	bText := "B-new-" + strings.Repeat("b", 40000)
	zText := "Z-already-delivered-" + strings.Repeat("z", 6000)
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapA, aText), stamp.Add(time.Minute))
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapB, bText), stamp.Add(2*time.Minute))
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapZ, zText), stamp.Add(-time.Minute))
	oldID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapB, pumpOverlapZ}, []string{"B-old", zText}, stamp, deliverStateAccepted)
	names, texts := []string{pumpOverlapA, pumpOverlapB}, []string{aText, bText}
	currentID := pumpQueueBatchID(thread, []string{pumpOverlapA, pumpOverlapB, pumpOverlapZ}, pumpReview776QueueBody(texts))
	pumpQueueTestPin(t, cfg, thread, currentID, names, texts)
	st := pumpTestReadStatePtr(t, cfg)
	pin := st.QueueAttempt[thread]
	pin.Base = currentID
	st.QueueAttempt[thread] = pin
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	if err := deliverSave(cfg, deliverRecord{LogicalID: currentID, RequestID: currentID, Tool: deliverToolSteer, TargetThread: thread,
		MessageSHA256: deliverMessageSHA256(pin.Body), CreatedAt: pumpQueueTestStamp(now), State: deliverStateAccepted}); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, "Z-already-delivered-"); n != 0 {
		t.Errorf("I1/R2: the accepted completion hid %s and z went %d more times", oldID, n)
	}
	if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
		t.Errorf("R2: a batch went while an unprovable record holds the thread: %d messages", len(messages))
	}
	if left, _ := pumpQueueSortedNames(dir); !reflect.DeepEqual(left, []string{pumpOverlapZ}) {
		t.Errorf("queued after the rounds: %v, want only z", left)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, pumpSentDir, name)); err != nil {
			t.Errorf("the accepted notice %s was not completed: %v", name, err)
		}
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); !ok || pin["held"] != true {
		t.Errorf("no held pin: %v", pin)
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyUnprovableHold) || !strings.Contains(logText, oldID) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyUnprovableHold, oldID, logText)
	}
}

// pumpUpgradeCancelAfterEntry reports the live state on its first Err call and cancels right after it,
// with the tree captured at that instant: it models a real cancellation racing the round's entry check.
type pumpUpgradeCancelAfterEntry struct {
	context.Context
	cancel  context.CancelFunc
	capture func()
	calls   int
}

func (c *pumpUpgradeCancelAfterEntry) Err() error {
	err := c.Context.Err()
	c.calls++
	if c.calls == 1 {
		c.cancel()
		c.capture()
	}
	return err
}

// A cancellation that lands after the round's entry check, before the pre-pin accepted membership is
// completed, makes no durable change: not even the sent/ directory (C7/D5).
func TestPumpQueueCancelledMembershipCompletionWritesNothing(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-bridge-991")
	thread := "parent-1"
	name := pumpOverlapA
	pumpQueueTestNotice(t, cfg, thread, name, "A-delivered")
	st := pumpTestReadStatePtr(t, cfg)
	st.QueueAccepted[thread] = []string{name}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	var before map[string]string
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &pumpUpgradeCancelAfterEntry{Context: parent, cancel: cancel}
	ctx.capture = func() { before = pumpQueueTreeSnapshot(t, cfg.StateDir) }
	err := pumpQueueFlushThread(ctx, e, cfg, st, pumpSettingsFrom(cfg), filepath.Join(cfg.StateDir, pumpQueueDir), thread, false)
	if err != context.Canceled {
		t.Fatalf("the completion returned %v, want the cancellation", err)
	}
	if after := pumpQueueTreeSnapshot(t, cfg.StateDir); !reflect.DeepEqual(before, after) {
		_, created := after[filepath.Join(pumpQueueDir, thread, pumpSentDir)+"/"]
		t.Errorf("C7/D5: the cancelled membership completion changed the tree; sent/ created=%v", created)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, thread, name)); err != nil {
		t.Errorf("the queued notice changed: %v", err)
	}
	// The helper itself refuses a cancelled context before it creates anything.
	done, stop := context.WithCancel(context.Background())
	stop()
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	if err := pumpReview776QueueMoveByName(done, dir, []string{name}); err != context.Canceled {
		t.Fatalf("MoveByName returned %v, want the cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(dir, pumpSentDir)); !os.IsNotExist(err) {
		t.Errorf("MoveByName created sent/ under a cancelled context: %v", err)
	}
}

// An overlap pin an earlier build built from fewer candidate sets answers only for [a,b]; the record
// over [b,z], accepted, also delivered z. The search runs before the pin completes a and b, finds [b,z]
// while b is still queued and folds it in, so z is completed and never sent again.
func TestPumpQueueEarlierOverlapPinGainsTheRecordItMissed(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapA, pumpOverlapB, pumpOverlapZ}
	texts := []string{"A-delivered", "B-delivered", "Z-delivered"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	abID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:2], texts[:2], stamp, deliverStateAccepted)
	pumpQueueTestLegacyRecord(t, cfg, thread, names[1:], texts[1:], stamp, deliverStateAccepted)
	st := pumpTestReadStatePtr(t, cfg)
	st.QueueAttempt[thread] = pumpReview776QueuePin{LogicalID: abID, Legacy: true, Names: names[:2],
		SHA256:  pumpReview776QueueDigests(names[:2], texts[:2]),
		Overlap: []pumpReview776QueueLegacyRef{{LogicalID: abID, Names: names[:2], Members: names[:2]}}}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
		t.Errorf("I1: a delivered notice went again: %q", messages)
	}
	if left, _ := pumpQueueSortedNames(dir); len(left) != 0 {
		t.Errorf("notices left queued: %v", left)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, pumpSentDir, name)); err != nil {
			t.Errorf("the delivered notice %s was not completed: %v", name, err)
		}
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); ok {
		t.Errorf("a pin is left: %v", pin)
	}
}

// A legacy pin an earlier build left without digests cannot prove its text; when the search also finds
// another record, the pin is folded in as an unprovable ref and the thread holds with its named line,
// while the other record's delivered notice is still completed.
func TestPumpQueueEarlierUnprovablePinHoldsWhenFolded(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapA, pumpOverlapB}
	texts := []string{"A-delivered", "B-unknown"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	pumpQueueTestLegacyRecord(t, cfg, thread, names[:1], texts[:1], stamp, deliverStateAccepted)
	oldPin := pumpQueueLegacyBatchID(thread, []string{pumpOverlapB, "0000000000000009.txt"})
	st := pumpTestReadStatePtr(t, cfg)
	st.QueueAttempt[thread] = pumpReview776QueuePin{LogicalID: oldPin, Legacy: true, Names: names[1:]}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
		t.Errorf("R2: a batch went under an unprovable pin: %q", messages)
	}
	if left, _ := pumpQueueSortedNames(dir); !reflect.DeepEqual(left, []string{pumpOverlapB}) {
		t.Errorf("queued after the rounds: %v, want only b", left)
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); !ok || pin["held"] != true {
		t.Errorf("no held pin: %v", pin)
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyUnprovableHold) || !strings.Contains(logText, oldPin) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyUnprovableHold, oldPin, logText)
	}
}
