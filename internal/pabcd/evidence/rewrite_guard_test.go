package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-593: RecordTombstone and ResolveTombstone read the session file with the strict reader and write what it rebuilt. The reader
// keeps 64 records and cuts a receiptClaimed to 256 units, so a file that stores more, or a field it changes, was rewritten
// shorter. Both now refuse, through state.RewriteKeepsUnverified, and the verdict goes to the marker tier.

func rewriteGuardRecords(n int, edit func(i int, m map[string]any)) []any {
	out := make([]any, n)
	for i := range out {
		m := map[string]any{"agentId": fmt.Sprintf("a%d", i+1), "turnId": "t1", "agentType": "executor", "attempts": 3, "receiptClaimed": "none", "recordedAt": "2026-01-01T00:00:00.000Z", "resolvable": true}
		if edit != nil {
			edit(i, m)
		}
		out[i] = m
	}
	return out
}

// rewriteGuardSeed writes a session file that stores list as it is, which is what a state written by the Node oracle or edited by hand
// can hold, and returns its bytes.
func rewriteGuardSeed(t *testing.T, cwd string, list []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"phase": "IDLE", "sessionId": "s1", "unverifiedSubagents": list})
	must(t, err)
	put(t, state.StatePath(cwd, "s1"), body)
	return body
}

func rewriteGuardFile(t *testing.T, cwd string) []byte {
	t.Helper()
	raw, err := os.ReadFile(state.StatePath(cwd, "s1"))
	must(t, err)
	return raw
}

func rewriteGuardStoredIDs(t *testing.T, raw []byte) (ids []string) {
	t.Helper()
	var f struct {
		Records []struct {
			AgentID string `json:"agentId"`
		} `json:"unverifiedSubagents"`
	}
	must(t, json.Unmarshal(raw, &f))
	for _, r := range f.Records {
		ids = append(ids, r.AgentID)
	}
	return ids
}

// The 65th verdict is stored (a state of 64 records is rewritten as it is), after which the reader sees 64 and the corruption flag:
// the 66th verdict used to be written over a file the reader had already shortened, which dropped the 65th. Now the 66th goes to the
// marker and the file is the one a65 left.
func TestRewriteGuardKeepsThe65thVerdictWhenThe66thArrives(t *testing.T) {
	cwd := t.TempDir()
	for i := 1; i <= 65; i++ {
		if !RecordTombstone(cwd, "s1", agent(fmt.Sprintf("a%d", i), "t1"), MaxAttempts, WriteUnrecordableMarker) {
			t.Fatalf("a%d was not recorded", i)
		}
	}
	after65 := rewriteGuardFile(t, cwd)
	if ids := rewriteGuardStoredIDs(t, after65); len(ids) != 65 || ids[64] != "a65" {
		t.Fatalf("after a65 the file holds %d records, last %q", len(ids), ids[len(ids)-1])
	}
	if RecordTombstone(cwd, "s1", agent("a66", "t1"), MaxAttempts, WriteUnrecordableMarker) {
		t.Error("the 66th verdict was reported recorded")
	}
	if after66 := rewriteGuardFile(t, cwd); !bytes.Equal(after66, after65) {
		t.Errorf("the file changed: a65 is %v", strings.Contains(string(after66), "a65"))
	}
	if got := UnrecordableVerdictStatus(cwd, "s1"); !got.Present {
		t.Errorf("the 66th verdict has no marker: %+v", got)
	}
}

// A stored list the reader would change is refused by every writer: the tombstone is not recorded (its verdict goes to the marker),
// nothing is resolved, and the file is byte for byte as it was.
func TestRewriteGuardRefusesAListTheReaderChanges(t *testing.T) {
	long := func(i int, m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen+1) }
	for name, list := range map[string][]any{
		"a receipt of 257 characters":         rewriteGuardRecords(1, long),
		"a receipt cut inside an astral char": rewriteGuardRecords(1, func(i int, m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", 255) + "\U0001F600b" }),
		"attempts stored as text":             rewriteGuardRecords(1, func(i int, m map[string]any) { m["attempts"] = "3" }),
		"a long receipt after a short one":    append(rewriteGuardRecords(1, nil), rewriteGuardRecords(2, long)[1]),
		"a malformed record beside a valid":   append(rewriteGuardRecords(1, nil), map[string]any{"bad": true}),
		"65 records":                          rewriteGuardRecords(65, nil),
	} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			before := rewriteGuardSeed(t, cwd, list)
			if RecordTombstone(cwd, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) {
				t.Error("a tombstone was recorded over a list the reader changes")
			}
			if got := UnrecordableVerdictStatus(cwd, "s1"); !got.Present {
				t.Errorf("the verdict has no marker: %+v", got)
			}
			if ResolveTombstone(cwd, "s1", agent("a1", "t1")) {
				t.Error("a verdict was resolved over a list the reader changes")
			}
			if after := rewriteGuardFile(t, cwd); !bytes.Equal(after, before) {
				t.Errorf("the file changed (%d bytes, was %d)", len(after), len(before))
			}
		})
	}
}

// The sentinel tier rewrites the same state, so it refuses the same way: the corruption flag is not written over a cut receipt, and
// the verdict reaches the marker writer.
func TestRewriteGuardSentinelTierRefuses(t *testing.T) {
	cwd, calls := t.TempDir(), 0
	before := rewriteGuardSeed(t, cwd, rewriteGuardRecords(1, func(i int, m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen+1) }))
	lock := func(dir, sessionID string, fn func() error) error {
		if calls++; calls == 1 {
			return errors.New("held")
		}
		return state.WithSessionLock(dir, sessionID, fn)
	}
	var marked []string
	marker := func(_, _, agentID string) error { marked = append(marked, agentID); return nil }
	if recordTombstone(cwd, "s1", agent("new", "t1"), 3, time.Now(), lock, marker) {
		t.Error("the verdict was reported recorded")
	}
	if after := rewriteGuardFile(t, cwd); !bytes.Equal(after, before) {
		t.Errorf("the sentinel tier rewrote the file (%d bytes, was %d)", len(after), len(before))
	}
	if calls != 2 || len(marked) != 1 || marked[0] != "new" {
		t.Errorf("lock calls %d, marker calls %v", calls, marked)
	}
}

// A list that the reader keeps whole is written as before: 64 short records resolve one and record another, and a missing file is
// created.
func TestRewriteGuardLeavesAWholeListAlone(t *testing.T) {
	cwd := t.TempDir()
	rewriteGuardSeed(t, cwd, rewriteGuardRecords(64, nil))
	if !ResolveTombstone(cwd, "s1", agent("a1", "t1")) {
		t.Error("a verdict of a list of 64 was not resolved")
	}
	if !RecordTombstone(cwd, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) {
		t.Error("a tombstone was not recorded over a list of 63")
	}
	if ids := rewriteGuardStoredIDs(t, rewriteGuardFile(t, cwd)); len(ids) != 64 || ids[0] != "a2" || ids[63] != "new" {
		t.Errorf("stored %v", ids)
	}
	fresh := t.TempDir()
	if !RecordTombstone(fresh, "s1", agent("a1", "t1"), MaxAttempts, WriteUnrecordableMarker) || len(rewriteGuardStoredIDs(t, rewriteGuardFile(t, fresh))) != 1 {
		t.Error("a missing file was not created")
	}
}
