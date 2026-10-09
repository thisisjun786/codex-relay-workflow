package manage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The post-r8 review: every ledger record for the thread that could have carried a text the queue may
// still send has to be answered -- by the search over the queue's names, or by the pin -- or the thread
// holds with a named line (R2). The tests below are the reviewer's three reproductions with the named
// line asserted, and the guards that keep the hold to records that could have carried such a text.

// An old-id pin an earlier build left froze [a,b] as A-old/B; the shorter accepted record over [a]
// delivered A-old. The producer then replaced a. The search must not dismiss the shorter record as stale
// on a's new time, because the pin's replay would send A-old again: the record may have carried that
// text, so it cannot prove its text against the queue, and the thread holds.
func TestPumpQueueOldIDPinFrozenTextKeepsItsOwnEvidence(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receipts, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapA, pumpOverlapB}
	oldTexts := []string{"A-already-delivered", "B-never-delivered"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, oldTexts[i]), stamp.Add(-time.Minute))
	}
	longID := pumpQueueTestLegacyRecord(t, cfg, thread, names, oldTexts, stamp, deliverStateUnknown)
	shortID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:1], oldTexts[:1], stamp, deliverStateAccepted)
	pumpOverlapSetReceipts(t, receipts, map[string]string{longID: pumpOverlapNotAttempted, shortID: pumpOverlapAccepted})
	pumpQueueTestPin(t, cfg, thread, longID, names, oldTexts)
	// A normal atomic producer replacement after the earlier pin was persisted.
	path := filepath.Join(cfg.StateDir, pumpQueueDir, thread, names[0])
	if err := os.WriteFile(path+".tmp", []byte("A-new-undelivered"), 0o600); err != nil {
		t.Fatal(err)
	}
	pumpQueueTestSetTime(t, path+".tmp", stamp.Add(time.Minute))
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, oldTexts[0]); n != 0 {
		t.Errorf("the old pin resent A %d times although the accepted %s delivered it: %q", n, shortID, pumpQueueTestSentMessages(t, log))
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyUnprovableHold) || !strings.Contains(logText, shortID) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyUnprovableHold, shortID, logText)
	}
}

// The same old-id pin, but a left the queue (an operator moved it) before this build ran: no set of the
// queue's names is the shorter record's id any more, and the pin would replay A-old. The record is one the
// pin does not answer for and could have carried the frozen text, so the thread holds.
func TestPumpQueueOldIDPinWithAMemberGoneHoldsOnTheOtherRecord(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(dir, pumpSentDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, names[0]), filepath.Join(dir, pumpSentDir, names[0])); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, texts[0]); n != 0 {
		t.Errorf("the old pin replayed A %d times although the accepted %s delivered it: %q", n, shortID, pumpQueueTestSentMessages(t, log))
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyUnmatchedHold) || !strings.Contains(logText, shortID) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyUnmatchedHold, shortID, logText)
	}
}

// An overlap pin an earlier build built from fewer candidate sets answers only for [a,b]. The accepted
// record over [b,z] also delivered z, and fourteen notices queued later take the queue past the search's
// limit. The pin is no proof that the search this build runs was complete, so the record it does not
// answer for holds the thread with the scan-limit line, and z is not sent again.
func TestPumpQueueEarlierOverlapPinOverTheLimitKeepsAllEvidence(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapA, pumpOverlapB, pumpOverlapZ}
	texts := []string{"A-delivered", "B-delivered", "Z-delivered"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	abID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:2], texts[:2], stamp, deliverStateAccepted)
	bzID := pumpQueueTestLegacyRecord(t, cfg, thread, names[1:], texts[1:], stamp, deliverStateAccepted)
	for i := 0; i < 14; i++ {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, fmt.Sprintf("%016x.txt", i+1), fmt.Sprintf("new-%02d", i)), stamp.Add(time.Minute))
	}
	st := pumpTestReadStatePtr(t, cfg)
	st.QueueAttempt[thread] = pumpReview776QueuePin{LogicalID: abID, Legacy: true, Names: names[:2],
		SHA256:  pumpReview776QueueDigests(names[:2], texts[:2]),
		Overlap: []pumpReview776QueueLegacyRef{{LogicalID: abID, Names: names[:2], Members: names[:2]}}}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, texts[2]); n != 0 {
		t.Errorf("the accepted %s was hidden by the over-limit overlap pin, and z went %d more times: %q", bzID, n, pumpQueueTestSentMessages(t, log))
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyScanLimitHold) || !strings.Contains(logText, bzID) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyScanLimitHold, bzID, logText)
	}
	st = pumpTestReadStatePtr(t, cfg)
	if st.QueueLegacyChecked[thread] {
		t.Errorf("a thread the search could not cover was marked searched")
	}
}

// A pre-change record over [b,z] whose b already left the queue before this build first ran is found by
// no set of the queue's names. It could have carried z (it was written after z), so it holds the thread
// with a named line instead of letting z go again (R2).
func TestPumpQueueLegacyRecordWithAMemberGoneHoldsTheThread(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receipts, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapB, pumpOverlapZ}
	texts := []string{"B-delivered", "Z-already-delivered"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	oldID := pumpQueueTestLegacyRecord(t, cfg, thread, names, texts, stamp, deliverStateUnknown)
	pumpOverlapSetReceipts(t, receipts, map[string]string{oldID: pumpOverlapAccepted})
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	if err := os.MkdirAll(filepath.Join(dir, pumpSentDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, names[0]), filepath.Join(dir, pumpSentDir, names[0])); err != nil {
		t.Fatal(err)
	}
	pumpLegacyHoldFlush(t, e, cfg, 3)
	if n := pumpOverlapSentCount(t, log, texts[1]); n != 0 {
		t.Errorf("R2: the unidentified pre-change record %s let its delivered z go again %d times: %q", oldID, n, pumpQueueTestSentMessages(t, log))
	}
	if logText := pumpQueueTestLog(t, cfg); !strings.Contains(logText, pumpQueueLegacyUnmatchedHold) || !strings.Contains(logText, oldID) {
		t.Errorf("no %s line naming %s:\n%s", pumpQueueLegacyUnmatchedHold, oldID, logText)
	}
	if left, _ := pumpQueueSortedNames(dir); len(left) != 1 || left[0] != pumpOverlapZ {
		t.Errorf("queued after the rounds: %v, want only z", left)
	}
}

// Guards on the hold: a direct send-parent delivery to the thread (its id is the digest of its own
// text, so it is no queue batch) and a record written before every queued notice (it could have carried
// none of them) are no reason to hold; the queue flows and each notice goes once.
func TestPumpQueueUnmatchedHoldSkipsRecordsThatCarriedNothingQueued(t *testing.T) {
	for _, kind := range []string{"direct", "older"} {
		t.Run(kind, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, _, log := pumpOverlapBridge(t)
			cfg := pumpTestConfig(t, bridge)
			thread := "parent-1"
			stamp := now.Add(-time.Hour)
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, pumpOverlapZ, "Z-queued"), stamp.Add(-time.Minute))
			switch kind {
			case "direct":
				text := "a direct notice"
				id := sendParentLogicalID([]byte(text))
				if err := deliverSave(cfg, deliverRecord{LogicalID: id, RequestID: id, Tool: deliverToolSend, TargetThread: thread,
					MessageSHA256: deliverMessageSHA256(text), CreatedAt: pumpQueueTestStamp(stamp), State: deliverStateAccepted}); err != nil {
					t.Fatal(err)
				}
			case "older":
				pumpQueueTestLegacyRecord(t, cfg, thread, []string{pumpOverlapB, pumpOverlapZ}, []string{"B", "Z-earlier"}, stamp.Add(-2*time.Hour), deliverStateAccepted)
			}
			pumpLegacyHoldFlush(t, e, cfg, 2)
			if n := pumpOverlapSentCount(t, log, "Z-queued"); n != 1 {
				t.Errorf("z sent %d times, want once: %q\n%s", n, pumpQueueTestSentMessages(t, log), pumpQueueTestLog(t, cfg))
			}
		})
	}
}

// An overlap pin this build took keeps the ids of the records its search matched and dismissed (here a
// refused record that proves its text), so a queue that grows past the search's limit under the pin is
// not held on them, and the notices go once the records settle.
func TestPumpQueueOverlapPinOverTheLimitKeepsItsDismissedRecords(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receipts, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	names := []string{pumpOverlapA, pumpOverlapB, pumpOverlapC}
	texts := []string{"A-delivered", "B-delivered", "C-refused"}
	for i, name := range names {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, name, texts[i]), stamp.Add(-time.Minute))
	}
	shortID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:1], texts[:1], stamp, deliverStateUnknown)
	longID := pumpQueueTestLegacyRecord(t, cfg, thread, names[:2], texts[:2], stamp, deliverStateUnknown)
	refusedID := pumpQueueTestLegacyRecord(t, cfg, thread, names[2:], texts[2:], stamp, deliverStateRefused)
	pumpOverlapSetReceipts(t, receipts, map[string]string{shortID: pumpOverlapUnknown, longID: pumpOverlapUnknown})
	pumpLegacyHoldFlush(t, e, cfg, 1)
	pin, ok := pumpReview776QueueAttempt(t, cfg, thread)
	if !ok {
		t.Fatalf("no overlap pin after the first round")
	}
	if judged, _ := pin["judged"].([]any); len(judged) != 1 || judged[0] != refusedID {
		t.Errorf("the overlap pin keeps judged=%v, want [%s]", pin["judged"], refusedID)
	}
	for i := 0; i < 14; i++ {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, fmt.Sprintf("%016x.txt", i+1), fmt.Sprintf("new-%02d", i)), stamp.Add(time.Minute))
	}
	pumpOverlapSetReceipts(t, receipts, map[string]string{shortID: pumpOverlapAccepted, longID: pumpOverlapAccepted})
	pumpLegacyHoldFlush(t, e, cfg, 4)
	logText := pumpQueueTestLog(t, cfg)
	if strings.Contains(logText, pumpQueueLegacyScanLimitHold) || strings.Contains(logText, pumpQueueLegacyUnmatchedHold) {
		t.Errorf("the queue was held on a record its own overlap pin had dismissed:\n%s", logText)
	}
	for _, text := range texts[:2] {
		if n := pumpOverlapSentCount(t, log, text); n != 0 {
			t.Errorf("I1: %s sent %d times: %q", text, n, pumpQueueTestSentMessages(t, log))
		}
	}
	if n := pumpOverlapSentCount(t, log, texts[2]); n != 1 {
		t.Errorf("I3: c sent %d times, want once: %q", n, pumpQueueTestSentMessages(t, log))
	}
	if left, _ := pumpQueueSortedNames(dir); len(left) != 0 {
		t.Errorf("notices left queued: %v", left)
	}
}
