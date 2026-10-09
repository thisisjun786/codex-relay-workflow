package evidence

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// tombstoneLockRetryLimit is how often a writer records its tombstone again after the session lock gave
// up. The lock's wait budget is the oracle's (state.WithSessionLock's LOCK_RETRY_DELAYS_MS,
// 5+10+15+20+25+30+35+40+35+35 ms = 250 ms in all) and giving up after it is deliberate, so the two
// writers below would otherwise be decided by how long one fsync takes on the machine running the
// test. Only that give-up is retried (CRW-861): any other failure is a failure of the test.
const tombstoneLockRetryLimit = 50

// tombstoneLockWatch wraps state.WithSessionLock for one recordTombstone call so the test can tell what
// the call's two lock acquisitions did: the first runs the commit, the second (only after the commit
// did not land) raises the corruption sentinel.
type tombstoneLockWatch struct {
	calls          int
	firstRan       bool
	firstErr       error
	sentinelRaised bool
}

func (w *tombstoneLockWatch) lock(cwd, sessionID string, fn func() error) error {
	w.calls++
	ran := false
	err := state.WithSessionLock(cwd, sessionID, func() error {
		ran = true
		return fn()
	})
	switch w.calls {
	case 1:
		w.firstRan, w.firstErr = ran, err
	case 2:
		w.sentinelRaised = ran && err == nil
	}
	return err
}

// gaveUp reports whether the commit never ran because the lock acquisition gave up, the only failure
// the test retries: the give-up returns the last create's fs.ErrExist and does not run fn.
func (w *tombstoneLockWatch) gaveUp() bool {
	return !w.firstRan && errors.Is(w.firstErr, fs.ErrExist)
}

// recordTombstoneRetryingLock records the verdict, calling RecordTombstone's body again only while the
// session lock gave up before the commit ran. A write error, a refused rewrite or an unreadable file
// fails at once with the first failure, which a later success would otherwise hide. giveUps counts the
// retried acquisitions and sentinels counts those whose sentinel tier then raised unverifiedCorrupt.
func recordTombstoneRetryingLock(t *testing.T, cwd, sessionID string, p Payload, giveUps, sentinels *atomic.Int32) bool {
	t.Helper()
	for attempt := 0; attempt <= tombstoneLockRetryLimit; attempt++ {
		w := &tombstoneLockWatch{}
		if recordTombstone(cwd, sessionID, p, MaxAttempts, time.Now(), w.lock, nil) {
			return true
		}
		if !w.gaveUp() {
			t.Errorf("RecordTombstone failed without the session lock giving up (commit ran %v, error %v)", w.firstRan, w.firstErr)
			return false
		}
		giveUps.Add(1)
		if w.sentinelRaised {
			sentinels.Add(1)
		}
	}
	t.Errorf("the session lock gave up %d times in a row", tombstoneLockRetryLimit+1)
	return false
}

// Two agents that stop at the same moment each commit their tombstone under the session lock: neither
// verdict is lost. Each writer holds the lock across a read-modify-write of the session file with
// fsync, so a writer that meets the lock giving up records again; that is the only failure retried. A
// give-up raises the corruption sentinel in the second tier (product behaviour, unchanged), so the
// session ends unverifiedCorrupt exactly when some give-up was followed by a sentinel that landed, and
// never otherwise.
func TestRecordTombstoneKeepsConcurrentVerdicts(t *testing.T) {
	cwd := t.TempDir()
	var wg sync.WaitGroup
	var giveUps, sentinels atomic.Int32
	results := make([]bool, 2)
	for i, agent := range []string{"racer-a", "racer-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = recordTombstoneRetryingLock(t, cwd, "s1", Payload{AgentType: "executor", AgentID: agent}, &giveUps, &sentinels)
		}()
	}
	wg.Wait()
	var ids []string
	final := state.ReadState(cwd, "s1")
	for _, e := range final.UnverifiedSubagents {
		ids = append(ids, e.AgentID)
	}
	if len(ids) != 2 || !results[0] || !results[1] || ids[0] == ids[1] {
		t.Errorf("results %v, tombstones %v", results, ids)
	}
	if want := sentinels.Load() > 0; final.UnverifiedCorrupt != want {
		t.Errorf("unverifiedCorrupt = %v, want %v (lock give-ups %d, sentinels raised %d)", final.UnverifiedCorrupt, want, giveUps.Load(), sentinels.Load())
	}
}

// Every tier that fails hands the verdict to the marker writer with the working directory, session and agent ids; a nil writer
// is a no-op.
func TestRecordTombstoneFallsBackToTheMarkerWriter(t *testing.T) {
	failing := func(string, string, func() error) error { return errors.New("held") }
	cwd, got := t.TempDir(), [][3]string{}
	marker := func(dir, sessionID, agentID string) error {
		got = append(got, [3]string{dir, sessionID, agentID})
		return errors.New("the writer failing changes nothing")
	}
	now := time.Now()
	p := Payload{AgentType: "executor", AgentID: "a1"}
	if recordTombstone(cwd, "s1", p, 3, now, failing, marker) || len(got) != 1 || got[0] != [3]string{cwd, "s1", "a1"} {
		t.Errorf("marker calls %v", got)
	}
	if recordTombstone(t.TempDir(), "s1", p, 3, now, failing, nil) {
		t.Error("a nil marker writer must not commit anything")
	}
}

// The sentinel tier reads the session file inside its own lock: a verdict another agent stored between the two lock
// acquisitions is still there afterwards.
func TestSentinelTierKeepsAVerdictStoredMeanwhile(t *testing.T) {
	cwd, calls := t.TempDir(), 0
	lock := func(dir, sessionID string, fn func() error) error {
		if calls++; calls == 1 {
			return errors.New("held")
		}
		RecordTombstone(dir, sessionID, Payload{AgentType: "worker", AgentID: "other"}, MaxAttempts, nil) // another agent stops meanwhile
		return state.WithSessionLock(dir, sessionID, fn)
	}
	if recordTombstone(cwd, "s1", Payload{AgentType: "executor", AgentID: "a1"}, 3, time.Now(), lock, nil) {
		t.Error("the first tier failed, so nothing is committed")
	}
	if s := state.ReadState(cwd, "s1"); !s.UnverifiedCorrupt || len(s.UnverifiedSubagents) != 1 || s.UnverifiedSubagents[0].AgentID != "other" {
		t.Errorf("sentinel %v, tombstones %v", s.UnverifiedCorrupt, s.UnverifiedSubagents)
	}
}

// An id that differs from another only in a lone surrogate cannot be told apart here: the JSON decode of the hook input turns
// each of them into U+FFFD, so both share one counter file. The oracle keeps them apart (test/subagent-evidence.test.ts:774).
func TestLoneSurrogateIdsShareACounter(t *testing.T) {
	var a, b struct{ ID string }
	if json.Unmarshal([]byte("{\"id\":\"\\ud800\"}"), &a) != nil || json.Unmarshal([]byte("{\"id\":\"\\ud801\"}"), &b) != nil {
		t.Fatal("decode")
	}
	want := filepath.Join("c", ".crw", AttemptsSubdir, "s-missing-"+tupleDigest("\ufffd", "")+".json")
	if a.ID != "\ufffd" || attemptsPath("c", "s", a.ID, "") != want || attemptsPath("c", "s", b.ID, "") != want || len(tupleDigest("\ufffd", "")) != 32 {
		t.Errorf("%q %q: %s", a.ID, b.ID, attemptsPath("c", "s", a.ID, ""))
	}
}

// receiptClaimed is cut at 256 UTF-16 units; a cut inside an astral character leaves a lone surrogate in the oracle and
// U+FFFD here, as the state package documents.
func TestReceiptClaimedIsCutAtUTF16Units(t *testing.T) {
	cwd := t.TempDir()
	msg := "EVIDENCE_RECORDED: " + strings.Repeat("p", 255) + "\U0001F600.md"
	if !RecordTombstone(cwd, "s1", Payload{AgentType: "executor", AgentID: "a1", LastAssistantMessage: msg}, 3, nil) {
		t.Fatal("not recorded")
	}
	if got := state.ReadState(cwd, "s1").UnverifiedSubagents[0].ReceiptClaimed; got != strings.Repeat("p", 255)+"\ufffd" {
		t.Errorf("%q", got)
	}
}
