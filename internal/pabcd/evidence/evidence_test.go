package evidence

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Two agents that stop at the same moment each commit their tombstone under the session lock: neither verdict is lost.
func TestRecordTombstoneKeepsConcurrentVerdicts(t *testing.T) {
	cwd := t.TempDir()
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i, agent := range []string{"racer-a", "racer-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = RecordTombstone(cwd, "s1", Payload{AgentType: "executor", AgentID: agent}, MaxAttempts, nil)
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

// Every tier that fails hands the verdict to the marker writer with the session and agent ids; a nil writer is a no-op.
func TestRecordTombstoneFallsBackToTheMarkerWriter(t *testing.T) {
	failing := func(string, string, func() error) error { return errors.New("held") }
	var got [][2]string
	marker := func(cwd, sessionID, agentID string) error {
		got = append(got, [2]string{sessionID, agentID})
		return errors.New("the writer failing changes nothing")
	}
	now := time.Now()
	p := Payload{AgentType: "executor", AgentID: "a1"}
	if recordTombstone(t.TempDir(), "s1", p, 3, now, failing, marker) || len(got) != 1 || got[0] != [2]string{"s1", "a1"} {
		t.Errorf("marker calls %v", got)
	}
	if recordTombstone(t.TempDir(), "s1", p, 3, now, failing, nil) {
		t.Error("a nil marker writer must not commit anything")
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

// A held lock is never broken: the commit fails, the other holder's lock survives, and nothing is written.
func TestRecordTombstoneNeverBreaksAHeldLock(t *testing.T) {
	cwd := t.TempDir()
	lock := filepath.Join(cwd, ".crw", "sessions", "s1.json.lock")
	p := Payload{AgentType: "executor", AgentID: "a1"}
	if !RecordTombstone(t.TempDir(), "s1", p, MaxAttempts, nil) {
		t.Fatal("control: nothing recorded without a held lock")
	}
	put(t, lock, []byte("12345"))
	if RecordTombstone(cwd, "s1", p, MaxAttempts, nil) {
		t.Error("recorded under a held lock")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("the other holder's lock was removed:", err)
	}
}
