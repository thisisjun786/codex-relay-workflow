package manage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pumpLegacyHoldFlush runs rounds of the queue with a fresh read of the state each round.
func pumpLegacyHoldFlush(t *testing.T, e *Env, cfg *Config, rounds int) {
	t.Helper()
	for round := 0; round < rounds; round++ {
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
	}
}

// An ordinary atomic replacement changes the age order, and a newly added notice that sorts first
// changes the name order, so the pre-change batch [b,z] is a prefix of neither: an accepted record over
// it must still be found. It cannot prove which text of z it carried (b's text is gone), so the thread
// holds with a named line listing the attempt, its members and why, and nothing is sent: not z again,
// and not the new a or b around the hold.
func TestPumpQueueLegacyBatchOutsideEveryPrefixHoldsTheThread(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receipts, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	b, z := pumpOverlapB, pumpOverlapZ
	bPath := pumpQueueTestNotice(t, cfg, thread, b, "B-old")
	zPath := pumpQueueTestNotice(t, cfg, thread, z, "Z-already-delivered")
	pumpQueueTestSetTime(t, bPath, stamp.Add(-2*time.Minute))
	pumpQueueTestSetTime(t, zPath, stamp.Add(-time.Minute))
	oldID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{b, z}, []string{"B-old", "Z-already-delivered"}, stamp, deliverStateUnknown)
	pumpOverlapSetReceipts(t, receipts, map[string]string{oldID: pumpOverlapAccepted})
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapA, "A-new"), stamp.Add(time.Minute))
	if err := os.WriteFile(bPath+".tmp", []byte("B-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestSetTime(t, bPath+".tmp", stamp.Add(2*time.Minute))
	if err := os.Rename(bPath+".tmp", bPath); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, "Z-already-delivered"); n != 0 {
		t.Errorf("the accepted pre-change member was sent again %d times: %q", n, pumpQueueTestSentMessages(t, log))
	}
	if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
		t.Errorf("a batch was sent while an unprovable pre-change record holds the thread: %q", messages)
	}
	for _, name := range []string{pumpOverlapA, b, z} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s left the queue under the hold: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, pumpSentDir, name)); err == nil {
			t.Errorf("%s reached sent/ although nothing proves which text the attempt carried", name)
		}
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); !ok || pin["held"] != true {
		t.Errorf("the thread is not held: %v", pin)
	}
	logText := pumpQueueTestLog(t, cfg)
	if !strings.Contains(logText, pumpQueueLegacyUnprovableHold) || !strings.Contains(logText, oldID) ||
		!strings.Contains(logText, b) || !strings.Contains(logText, z) {
		t.Errorf("no %s line naming %s and its members:\n%s", pumpQueueLegacyUnprovableHold, oldID, logText)
	}
}

// A pre-change record that cannot prove its text never authorises a resend, whatever its receipt
// says: the thread holds for the operator. The record [a,b] carried b by time alone (a was written
// again after it), so a receipt that says it never went does not show which text of b went nowhere.
func TestPumpQueueUnprovableLegacyReceiptNeverAuthorisesAResend(t *testing.T) {
	for _, kind := range []string{pumpOverlapNotAttempted, pumpOverlapRefused, pumpOverlapUnknown, pumpOverlapUnreachable, pumpOverlapAccepted} {
		t.Run(kind, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, receipts, log := pumpOverlapBridge(t)
			cfg := pumpTestConfig(t, bridge)
			thread := "parent-1"
			stamp := now.Add(-time.Hour)
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapA, "A-new"), stamp.Add(time.Minute))
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapB, "B-kept"), stamp.Add(-time.Minute))
			oldID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapA, pumpOverlapB}, []string{"A-old", "B-kept"}, stamp, deliverStateUnknown)
			pumpOverlapSetReceipts(t, receipts, map[string]string{oldID: kind})
			pumpLegacyHoldFlush(t, e, cfg, 3)
			if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
				t.Errorf("an unprovable record's %s receipt let a batch go: %q", kind, messages)
			}
			if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); !ok || pin["held"] != true {
				t.Errorf("the thread is not held: %v", pin)
			}
			if !strings.Contains(pumpQueueTestLog(t, cfg), pumpQueueLegacyUnprovableHold) {
				t.Errorf("no %s line:\n%s", pumpQueueLegacyUnprovableHold, pumpQueueTestLog(t, cfg))
			}
		})
	}
}

// A provable pre-change record held beside an unprovable one still completes the notice it proves
// delivered, and the thread holds for the rest.
func TestPumpQueueUnprovableHoldStillCompletesAProvenNotice(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receipts, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapA, "A-new"), stamp.Add(time.Minute))
	pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapB, "B-kept"), stamp.Add(-2*time.Minute))
	partial := pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapA, pumpOverlapB}, []string{"A-old", "B-kept"}, stamp, deliverStateUnknown)
	whole := pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapB}, []string{"B-kept"}, stamp.Add(-time.Minute), deliverStateUnknown)
	pumpOverlapSetReceipts(t, receipts, map[string]string{partial: pumpOverlapUnknown, whole: pumpOverlapUnknown})
	pumpLegacyHoldFlush(t, e, cfg, 2)
	if _, err := os.Lstat(filepath.Join(dir, pumpOverlapB)); err != nil {
		t.Fatalf("b left the queue before any record answered: %v", err)
	}
	// The whole record's receipt settles as accepted on a later round: b is proven and completed under
	// the hold, a stays queued and nothing is sent.
	pumpOverlapSetReceipts(t, receipts, map[string]string{partial: pumpOverlapNotAttempted, whole: pumpOverlapAccepted})
	pumpLegacyHoldFlush(t, e, cfg, 2)
	if _, err := os.Lstat(filepath.Join(dir, pumpSentDir, pumpOverlapB)); err != nil {
		t.Errorf("b, proven delivered by the whole record, was not completed under the hold: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, pumpOverlapA)); err != nil {
		t.Errorf("a left the queue under the hold: %v", err)
	}
	if messages := pumpQueueTestSentMessages(t, log); len(messages) != 0 {
		t.Errorf("a batch was sent under the hold: %q", messages)
	}
	if pin, ok := pumpReview776QueueAttempt(t, cfg, thread); !ok || pin["held"] != true {
		t.Errorf("the thread is not held: %v", pin)
	}
}

// pumpLegacyHoldCancelAtDeliver cancels the round at Deliver's entry, after the queue saved its pin and
// before anything reached a bridge: the second Err call is Deliver's own check.
type pumpLegacyHoldCancelAtDeliver struct {
	context.Context
	cancel  context.CancelFunc
	checks  int
	capture func()
}

func (c *pumpLegacyHoldCancelAtDeliver) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
		c.capture()
	}
	return c.Context.Err()
}

// A round cancelled at Deliver's entry has made the pin durable and sent nothing. The pin stays, and
// the queue changes nothing after the cancellation.
func TestPumpQueueCancelBeforeDeliverKeepsThePin(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-991-bridge")
	thread, name := "parent-1", pumpOverlapA
	pumpQueueTestNotice(t, cfg, thread, name, "A")
	st := pumpTestReadStatePtr(t, cfg)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &pumpLegacyHoldCancelAtDeliver{Context: parent, cancel: cancel}
	var atCancel map[string]string
	ctx.capture = func() { atCancel = pumpQueueTreeSnapshot(t, cfg.StateDir) }
	batch := pumpReview776QueueBatch{names: []string{name}, texts: []string{"A"}, body: "A", idNames: []string{name}}
	err := pumpQueueSend(ctx, e, cfg, st, filepath.Join(cfg.StateDir, pumpQueueDir, thread), thread, batch)
	if !errors.Is(err, context.Canceled) || parent.Err() == nil || atCancel == nil {
		t.Fatalf("the cancellation was not reached: err=%v checks=%d", err, ctx.checks)
	}
	if after := pumpQueueTreeSnapshot(t, cfg.StateDir); !reflect.DeepEqual(atCancel, after) {
		t.Errorf("the queue changed durable state after the cancellation: at cancel %v, after %v", atCancel[pumpStateFile], after[pumpStateFile])
	}
	if _, pinned := pumpReview776QueueAttempt(t, cfg, thread); !pinned {
		t.Errorf("the durable pin was lifted after the cancellation, although nothing was sent")
	}
}

// A cancelled round lifts no pin through any lift path: the local refusal, the ledger refusal and the
// overlap settlement all keep the stored state as it was.
func TestPumpQueuePinLiftDoesNothingAfterCancellation(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "/nonexistent/crw-991-bridge")
	_ = e
	thread := "parent-1"
	st := pumpTestReadStatePtr(t, cfg)
	st.QueueAttempt[thread] = pumpReview776QueuePin{LogicalID: "id-1", Names: []string{pumpOverlapA}, Body: "A", SHA256: map[string]string{pumpOverlapA: pumpReview776BodyDigest("A")}}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cause := fmt.Errorf("local refusal")
	if err := pumpReview776QueuePinLift(ctx, cfg, st, thread, cause); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Errorf("a cancelled lift answered %v, want the cause and the cancellation", err)
	}
	if _, pinned := st.QueueAttempt[thread]; !pinned {
		t.Errorf("a cancelled lift dropped the pin in memory")
	}
	if _, pinned := pumpReview776QueueAttempt(t, cfg, thread); !pinned {
		t.Errorf("a cancelled lift dropped the durable pin")
	}
}

// A queue too large for the scan of pre-change attempts holds while the ledger keeps a record for the
// thread that could have carried a queued notice, with a named line; it flows when no such record is
// there.
func TestPumpQueueLegacyScanLimitHoldsOnlyWithACandidateRecord(t *testing.T) {
	for _, withRecord := range []bool{false, true} {
		t.Run(fmt.Sprintf("record=%v", withRecord), func(t *testing.T) {
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
			if withRecord {
				pumpQueueTestLegacyRecord(t, cfg, thread, names[:2], []string{"N00", "N01"}, stamp, deliverStateUnknown)
			}
			pumpLegacyHoldFlush(t, e, cfg, 2)
			sent := pumpQueueTestSentMessages(t, log)
			logText := pumpQueueTestLog(t, cfg)
			if withRecord {
				if len(sent) != 0 {
					t.Errorf("a batch was sent while an unidentified pre-change record may cover it: %q", sent)
				}
				if !strings.Contains(logText, pumpQueueLegacyScanLimitHold) {
					t.Errorf("no %s line:\n%s", pumpQueueLegacyScanLimitHold, logText)
				}
				return
			}
			if len(sent) == 0 {
				t.Errorf("a queue with no pre-change record was held: %s", logText)
			}
		})
	}
}

// The search for pre-change records runs until it finds nothing once: that marks the thread, the mark
// is written with the batch's own pin, and a held thread is never marked.
func TestPumpQueueLegacySearchMarksTheThreadOnlyWhenItFindsNothing(t *testing.T) {
	for _, withRecord := range []bool{false, true} {
		t.Run(fmt.Sprintf("record=%v", withRecord), func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, _, _ := pumpOverlapBridge(t)
			cfg := pumpTestConfig(t, bridge)
			thread := "parent-1"
			stamp := now.Add(-time.Hour)
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapA, "A-new"), stamp.Add(time.Minute))
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapB, "B-kept"), stamp.Add(-time.Minute))
			if withRecord {
				pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapA, pumpOverlapB}, []string{"A-old", "B-kept"}, stamp, deliverStateUnknown)
			}
			pumpLegacyHoldFlush(t, e, cfg, 1)
			st, err := pumpLoadState(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if st.QueueLegacyChecked[thread] == withRecord {
				t.Errorf("the thread's search mark is %v with a pre-change record present=%v", st.QueueLegacyChecked[thread], withRecord)
			}
		})
	}
}
