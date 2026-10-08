package evidence

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-815: RecordTombstone and ResolveTombstone rewrite the whole session state from the strict reader's
// rebuilt value, and until this issue both checked only the unverified-subagent list. A stored interview
// tracker longer than interview.MaxTrackerArray is cut by ReconstructInterview (drop-oldest), so those
// writes lost the oldest records for good. Red on the base (both write and lose the tracker); after the
// fix both leave the file byte for byte. The CRW-648 allowance for a legacy D-close marker is unchanged.

// rewriteStoredEvidenceTracker is a stored contradictions array of n valid records.
func rewriteStoredEvidenceTracker(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"contradictionId":"k%d","severity":"high","summary":"s%d"}`, i, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// rewriteStoredEvidenceSeed writes a session file whose stored interview tracker is one past the cap, with
// list as the stored unverifiedSubagents array, and returns the file's bytes.
func rewriteStoredEvidenceSeed(t *testing.T, cwd, list string) []byte {
	t.Helper()
	body := `{"phase":"IDLE","sessionId":"s1","interview":{"contradictions":` +
		rewriteStoredEvidenceTracker(interview.MaxTrackerArray+1) + `,"assumptions":[]},"unverifiedSubagents":` + list + `}`
	put(t, state.StatePath(cwd, "s1"), []byte(body))
	return []byte(body)
}

// TestEvidenceRefusesToRewriteALongInterviewTracker is the tombstone and resolve case: neither write may
// drop the oldest contradictions, and the file must stay as it was.
func TestEvidenceRefusesToRewriteALongInterviewTracker(t *testing.T) {
	t.Run("record", func(t *testing.T) {
		cwd := t.TempDir()
		before := rewriteStoredEvidenceSeed(t, cwd, "[]")
		if RecordTombstone(cwd, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) {
			t.Error("a tombstone was recorded over a tracker the reader cuts")
		}
		if after := rewriteGuardFile(t, cwd); !bytes.Equal(after, before) {
			t.Errorf("the file changed (%d bytes, was %d)", len(after), len(before))
		}
	})
	t.Run("resolve", func(t *testing.T) {
		cwd := t.TempDir()
		before := rewriteStoredEvidenceSeed(t, cwd, `[{"agentId":"a1","turnId":"t1","agentType":"executor","attempts":3,"receiptClaimed":"none","recordedAt":"2026-01-01T00:00:00.000Z","resolvable":true}]`)
		if ResolveTombstone(cwd, "s1", agent("a1", "t1")) {
			t.Error("a verdict was resolved over a tracker the reader cuts")
		}
		if after := rewriteGuardFile(t, cwd); !bytes.Equal(after, before) {
			t.Errorf("the file changed (%d bytes, was %d)", len(after), len(before))
		}
	})
}

// TestEvidenceStillWritesATrackerItKeepsWhole is the other half: a tracker within the cap still writes, so
// the guard refuses only a real loss.
func TestEvidenceStillWritesATrackerItKeepsWhole(t *testing.T) {
	cwd := t.TempDir()
	body := `{"phase":"IDLE","sessionId":"s1","interview":{"contradictions":` +
		rewriteStoredEvidenceTracker(2) + `,"assumptions":[]},"unverifiedSubagents":[]}`
	put(t, state.StatePath(cwd, "s1"), []byte(body))
	if !RecordTombstone(cwd, "s1", agent("new", "t1"), MaxAttempts, WriteUnrecordableMarker) {
		t.Error("a tombstone was not recorded over a tracker within the cap")
	}
	if _, err := os.ReadFile(state.StatePath(cwd, "s1")); err != nil {
		t.Fatal(err)
	}
}
