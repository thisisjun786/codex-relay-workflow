package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Oracle: fallback-dispatch.test.ts:90-118,133-143,151-157; standalone
// issueManagedSpawn recorded with the pinned oracle before implementation.
func TestManagedSpawnRoleAndFallback(t *testing.T) {
	for _, role := range Roles() {
		t.Run(string(role), func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			_, err := SetRole(env, role, RolePatch{Mode: Some(ModeModel), Model: Some("primary/model"), Effort: Some(EffortHigh), Fallback: Some(FallbackPatch{Model: Some("fallback/model"), Effort: Null[EffortName]()})})
			check(t, err)
			start := dispatchTestCall(t, ws, env, map[string]any{"action": "start", "role": string(role)})
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			next := dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "failed", "error": "insufficient_quota", "executionState": "not_created", "reconciliation": "no child was created"})
			claim := dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": next.AttemptID})
			for _, ending := range []string{"", "\nTASK", "\r\nTASK", "\rTASK", "\u2028TASK", "\u2029TASK"} {
				for _, prefix := range []string{"", "earlier\n", "earlier\r", "earlier\u2028", "earlier\u2029"} {
					got, err := ManagedSpawn(ws, "session-test", prefix+claim.Marker+ending)
					check(t, err)
					if got == nil || got.Role != role || got.Candidate.Model == nil || *got.Candidate.Model != "fallback/model" || got.Candidate.Effort != nil {
						t.Fatalf("marker selection = %+v", got)
					}
				}
			}
		})
	}
}

func TestManagedSpawnRefusalsAndNoMarker(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	before := must(os.ReadFile(file))
	for _, msg := range []string{"TASK", "inline [CRW-DISPATCH:task-test:x]", "[CRW-DISPATCH:task-test:x] suffix"} {
		got, err := ManagedSpawn(ws, "invalid/session", msg)
		check(t, err)
		if got != nil {
			t.Fatal("unmarked message selected a candidate")
		}
	}
	if string(must(os.ReadFile(file))) != string(before) {
		t.Fatal("unmarked read wrote state")
	}
	marker := "[CRW-DISPATCH:task-test:" + start.AttemptID + "]"
	if _, err := ManagedSpawn(ws, "session-test", marker); err == nil {
		t.Fatal("unclaimed marker accepted")
	}
	claim := dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	for _, msg := range []string{"[CRW-DISPATCH:task-test:stale]", "[CRW-DISPATCH:_bad:x]"} {
		if _, err := ManagedSpawn(ws, "session-test", msg); err == nil {
			t.Fatal("invalid/current marker accepted")
		}
	}
	if _, err := ManagedSpawn(ws, "other", claim.Marker); err == nil {
		t.Fatal("foreign session selected state")
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "failed", "error": "permission_denied"})
	if _, err := ManagedSpawn(ws, "session-test", claim.Marker); err == nil {
		t.Fatal("terminal marker accepted")
	}
}

func TestManagedSpawnIssuancePersistsAndReplays(t *testing.T) {
	for _, hasID := range []bool{true, false} {
		t.Run(map[bool]string{true: "host-id", false: "no-host-id"}[hasID], func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			claim := dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			var raw map[string]any
			check(t, json.Unmarshal(must(os.ReadFile(file)), &raw))
			raw["foreign"] = "kept"
			raw["attempts"].([]any)[0].(map[string]any)["foreign"] = "kept"
			check(t, os.WriteFile(file, must(json.Marshal(raw)), 0600))
			var id *string
			if hasID {
				s := "call-1"
				id = &s
			}
			got, err := IssueManagedSpawn(ws, "session-test", claim.Marker+"\nTASK", id)
			check(t, err)
			if got == nil {
				t.Fatal("issuance absent")
			}
			stored := must(dispatchRead(file, "session-test", "task-test"))
			a := stored.Attempts[0]
			if !a.SpawnIssued || (hasID && (a.ToolUseID == nil || *a.ToolUseID != "call-1")) {
				t.Fatalf("issuance not stored: %+v", a)
			}
			if !strings.Contains(string(must(os.ReadFile(file))), `"foreign": "kept"`) {
				t.Fatal("foreign record member lost")
			}
			_, err = IssueManagedSpawn(ws, "session-test", claim.Marker, id)
			if hasID {
				check(t, err)
			} else if err == nil {
				t.Fatal("nil tool ID replay accepted")
			}
			other := "call-2"
			before := must(os.ReadFile(file))
			if _, err := IssueManagedSpawn(ws, "session-test", claim.Marker, &other); err == nil {
				t.Fatal("second native call accepted")
			}
			if string(before) != string(must(os.ReadFile(file))) {
				t.Fatal("refused issuance changed state")
			}
		})
	}
}

func TestManagedSpawnHeldLockAndUnmarked(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	claim := dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	check(t, os.Mkdir(file+".lock", 0700))
	before := must(os.ReadFile(file))
	id := "call"
	if _, err := IssueManagedSpawn(ws, "session-test", claim.Marker, &id); err == nil {
		t.Fatal("held lock accepted")
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("held lock changed state")
	}
	got, err := IssueManagedSpawn(ws, "bad/session", "TASK", nil)
	check(t, err)
	if got != nil {
		t.Fatal("unmarked issuance")
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw", "dispatches", "bad")); !os.IsNotExist(err) {
		t.Fatal("unmarked issuance created state")
	}
}
