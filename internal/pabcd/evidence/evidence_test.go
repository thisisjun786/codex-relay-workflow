package evidence

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// tombstoneLockRetryLimit is how often the widened lock below calls the session lock again after its
// wait budget is exhausted.
const tombstoneLockRetryLimit = 50

// tombstoneWideBudgetLock is the session lock with a wider budget than the oracle's, built from the
// test side. The oracle's lock (state.WithSessionLock, LOCK_RETRY_DELAYS_MS, 5+10+15+20+25+30+35+40+35+35
// ms = 250 ms in all) returns the last create error when it cannot be had, and giving up there is
// deliberate; a slow CI disk can spend that budget on one fsync, which is what this test must not
// depend on. RecordTombstone takes no lock argument, so the test drives the package's own seam
// (recordTombstone) the way the tests beside it do, and hands it this lock. The product's budget is
// untouched, and a lock that is genuinely stuck still refuses after the limit.
func tombstoneWideBudgetLock() lockFunc {
	return func(cwd, sessionID string, fn func() error) error {
		for attempt := 0; ; attempt++ {
			entered := false
			err := state.WithSessionLock(cwd, sessionID, func() error { entered = true; return fn() })
			if entered || !errors.Is(err, fs.ErrExist) || attempt >= tombstoneLockRetryLimit {
				return err
			}
		}
	}
}

// Two agents that stop at the same moment each commit their tombstone under the session lock: neither
// verdict is lost. Each writer holds the lock across a read-modify-write of the session file with
// fsync, so they run under tombstoneWideBudgetLock rather than the oracle's 250 ms budget.
func TestRecordTombstoneKeepsConcurrentVerdicts(t *testing.T) {
	cwd := t.TempDir()
	lock := tombstoneWideBudgetLock()
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i, agent := range []string{"racer-a", "racer-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = recordTombstone(cwd, "s1", Payload{AgentType: "executor", AgentID: agent}, MaxAttempts, time.Now(), lock, nil)
		}()
	}
	wg.Wait()
	var ids []string
	for _, e := range state.ReadState(cwd, "s1").UnverifiedSubagents {
		ids = append(ids, e.AgentID)
	}
	if len(ids) != 2 || !results[0] || !results[1] || ids[0] == ids[1] {
		t.Errorf("results %v, tombstones %v", results, ids)
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
