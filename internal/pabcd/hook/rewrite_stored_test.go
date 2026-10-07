package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-815: the memory gate and the idle-edit counter rewrite the whole session state from the strict
// reader's rebuilt value, and until this issue both checked only the unverified-subagent list. A stored
// interview tracker longer than interview.MaxTrackerArray is cut by ReconstructInterview (drop-oldest),
// so those two writes lost the oldest records for good. These cases are red on the base: both write and
// lose the tracker; after the fix both leave the file byte for byte.

// rewriteStoredTracker is a stored contradictions array of n valid records.
func rewriteStoredTracker(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"contradictionId":"k%d","severity":"high","summary":"s%d"}`, i, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// rewriteStoredInterview is a stored interview value holding a tracker one past the cap.
func rewriteStoredInterview() string {
	return `{"contradictions":` + rewriteStoredTracker(interview.MaxTrackerArray+1) + `,"assumptions":[]}`
}

// rewriteStoredPatch replaces the interview key of the session file at path and returns the new bytes.
func rewriteStoredPatch(t *testing.T, path, tracker string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal([]byte(tracker), &v); err != nil {
		t.Fatal(err)
	}
	m["interview"] = v
	stored, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stored, 0o644); err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestMemoryGateRefusesToRewriteALongInterviewTracker is the grant-write case: a spendable authorization
// and a stored tracker past the cap. The spend must be refused and the file left as it was, so the
// authorization survives for a repaired state.
func TestMemoryGateRefusesToRewriteALongInterviewTracker(t *testing.T) {
	cwd, _, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	before := rewriteStoredPatch(t, state.StatePath(cwd, gateSession), rewriteStoredInterview())
	reason := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env))
	if !strings.Contains(reason, "cannot rewrite") {
		t.Errorf("reason: %s", reason)
	}
	if after, _ := os.ReadFile(state.StatePath(cwd, gateSession)); string(after) != string(before) {
		t.Errorf("the state was rewritten: %s", after)
	}
	if s := state.ReadState(cwd, gateSession); !s.MemoryWriteGrant {
		t.Errorf("a refused call spent the authorization: %+v", s)
	}
}

// rewriteStoredIdleEnv is HOME, CODEX_HOME and CRW_HOME in a temporary directory.
func rewriteStoredIdleEnv(t *testing.T) (host.LookupEnv, string) {
	t.Helper()
	dir := t.TempDir()
	m := map[string]string{"HOME": dir, "CODEX_HOME": dir, "CODEX_SQLITE_HOME": dir, "CRW_BIN": "crw"}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }, dir
}

// rewriteStoredIdlePayload is the PreToolUse payload the idle-edit advisory reads.
func rewriteStoredIdlePayload(cwd, sid, tool string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": sid, "cwd": cwd, "tool_name": tool})
	return string(b)
}

// TestIdleEditRefusesToRewriteALongInterviewTracker is the counter-write case: the advisory still
// answers, the counter is not written, and the stored tracker is untouched.
func TestIdleEditRefusesToRewriteALongInterviewTracker(t *testing.T) {
	env, _ := rewriteStoredIdleEnv(t)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(state.StatePath(cwd, "s1")), 0o777); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"phase": "IDLE", "sessionId": "s1", "loopArmSeen": true, "idleEditNudges": 0, "unverifiedSubagents": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.StatePath(cwd, "s1"), body, 0o666); err != nil {
		t.Fatal(err)
	}
	before := rewriteStoredPatch(t, state.StatePath(cwd, "s1"), rewriteStoredInterview())
	if out := HandleIdleEditAdvisory(rewriteStoredIdlePayload(cwd, "s1", "apply_patch"), env); out == "" {
		t.Error("a skipped counter hid the advisory")
	}
	if after, err := os.ReadFile(state.StatePath(cwd, "s1")); err != nil || string(after) != string(before) {
		t.Errorf("the counter write changed the file (%d bytes, was %d; %v)", len(after), len(before), err)
	}
}
