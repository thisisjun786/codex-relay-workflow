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

// CRW-1110: the 65th and 66th verdicts are recorded beside the main list, which keeps the 64 the reader reads and is never flagged
// as overflowed; each verdict is found and resolved where it is. (Before CRW-1110 the 65th was appended to the file, the reader then
// saw 64 and the corruption flag, and the 66th could only reach the marker.)
func TestRewriteGuardKeepsThe65thVerdictWhenThe66thArrives(t *testing.T) {
	cwd := t.TempDir()
	for i := 1; i <= 66; i++ {
		if !RecordTombstone(cwd, "s1", agent(fmt.Sprintf("a%d", i), "t1"), MaxAttempts, WriteUnrecordableMarker) {
			t.Fatalf("a%d was not recorded", i)
		}
	}
	if ids := rewriteGuardStoredIDs(t, rewriteGuardFile(t, cwd)); len(ids) != 64 || ids[63] != "a64" {
		t.Fatalf("the main list holds %d records, last %q", len(ids), ids[len(ids)-1])
	}
	if s, _ := state.ReadStateStrict(cwd, "s1"); s.UnverifiedCorrupt {
		t.Error("the main list reads as overflowed")
	}
	for _, id := range []string{"a1", "a64", "a65", "a66"} {
		if !HasTombstone(cwd, "s1", agent(id, "t1")) {
			t.Errorf("%s is not found", id)
		}
	}
	if !ResolveTombstone(cwd, "s1", agent("a66", "t1")) || HasTombstone(cwd, "s1", agent("a66", "t1")) || !HasTombstone(cwd, "s1", agent("a65", "t1")) {
		t.Error("resolving a66 did not resolve exactly a66")
	}
	if got := UnrecordableVerdictStatus(cwd, "s1"); got.Present {
		t.Errorf("a verdict went to the marker: %+v", got)
	}
}

// CRW-1110: a file written before the fix with 65 whole records is recovered by the next writer: the 65th moves beside the main
// list and nothing is dropped. One with a record the reader would change is still refused, byte for byte.
func TestRewriteGuardRecoversA65RecordFile(t *testing.T) {
	cwd := t.TempDir()
	rewriteGuardSeed(t, cwd, rewriteGuardRecords(65, nil))
	if !RecordTombstone(cwd, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) {
		t.Fatal("a tombstone was not recorded after the recovery")
	}
	if ids := rewriteGuardStoredIDs(t, rewriteGuardFile(t, cwd)); len(ids) != 64 || ids[63] != "a64" {
		t.Fatalf("recovered main list %v", ids)
	}
	for _, id := range []string{"a1", "a64", "a65", "new"} {
		if !HasTombstone(cwd, "s1", agent(id, "t1")) {
			t.Errorf("%s was lost", id)
		}
	}
	if s, _ := state.ReadStateStrict(cwd, "s1"); s.UnverifiedCorrupt {
		t.Error("the recovered list still reads as overflowed")
	}
	long := func(i int, m map[string]any) {
		if i == 64 {
			m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen+1)
		}
	}
	bad := t.TempDir()
	before := rewriteGuardSeed(t, bad, rewriteGuardRecords(65, long))
	if RecordTombstone(bad, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) || ResolveTombstone(bad, "s1", agent("a1", "t1")) {
		t.Error("a 65-record file with a record the reader changes was rewritten")
	}
	if after := rewriteGuardFile(t, bad); !bytes.Equal(after, before) {
		t.Error("the file changed")
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
