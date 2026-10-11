package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1260 (pre-merge evaluation of 91fb1493): a V2 task is host ciphertext, so the spawn hook cannot read the attempt's marker in
// it. The claim is where the caller opts the attempt in to being named by the native call's task_name (v2TaskName:true); the
// record keeps the opt-in, the claim's answer names the task_name to use, and a claim without it keeps the oracle's record.
func TestDispatchClaimV2TaskNameOptIn(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestError(t, ws, env, map[string]any{"sessionId": "session-test", "dispatchId": "task-test", "action": "claim", "attemptId": start.AttemptID, "v2TaskName": "yes"}, "v2TaskName must be true or false")
	if strings.Contains(string(must(os.ReadFile(file))), `"claimed":true`) {
		t.Fatal("a refused claim claimed the attempt")
	}
	claim := dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID, "v2TaskName": true})
	answer, err := json.Marshal(claim)
	check(t, err)
	if claim.Action != "spawn" || !strings.Contains(string(answer), `"taskName":"task-test"`) {
		t.Fatalf("claim answer = %s", answer)
	}
	var record struct {
		Attempts []map[string]any `json:"attempts"`
	}
	check(t, json.Unmarshal(must(os.ReadFile(file)), &record))
	if record.Attempts[0]["v2TaskName"] != true {
		t.Fatalf("the opt-in was not recorded: %v", record.Attempts[0])
	}
	// The record still reads, and a status shows the opt-in.
	if status, err := json.Marshal(dispatchTestCall(t, ws, env, map[string]any{"action": "status"})); err != nil || !strings.Contains(string(status), `"v2TaskName":true`) {
		t.Fatalf("the opt-in was not read back: %s", status)
	}

	// A claim without the opt-in (or with false) writes the oracle's attempt and answers without a task name.
	for _, value := range []any{nil, false} {
		ws, env, start, file := dispatchTestFixture(t)
		fields := map[string]any{"action": "claim", "attemptId": start.AttemptID}
		if value != nil {
			fields["v2TaskName"] = value
		}
		answer, err := json.Marshal(dispatchTestCall(t, ws, env, fields))
		check(t, err)
		if strings.Contains(string(answer), "taskName") || strings.Contains(string(must(os.ReadFile(file))), "v2TaskName") {
			t.Fatalf("a claim without the opt-in (%v) carries it: %s", value, answer)
		}
	}
}

// The SessionStart notice states the V2 rule of the managed path, after the CXC guidance and the CRW-1257 sentence, within the
// notice's 4096-unit budget.
func TestFallbackNoticeStatesTheV2TaskNameRule(t *testing.T) {
	const rule = "CRW V2: claim with v2TaskName:true and spawn with task_name equal to the dispatchId."
	m, env := fallbackTestEnv(t)
	roles := `{"explorer":{"fallback":{"model":"p/f"}},"reviewer":{"fallback":{"model":"p/f"}},"executor":{"fallback":{"model":"p/f"}},"architect":{"fallback":{"model":"p/f"}}}`
	check(t, os.WriteFile(filepath.Join(m["CRW_HOME"], StoreFile), []byte(`{"roles":`+roles+`}`), 0600))
	notice, err := SessionFallbackNotice(env)
	check(t, err)
	var out struct {
		Hook struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	check(t, json.Unmarshal([]byte(notice), &out))
	if !strings.Contains(out.Hook.Context, ExplicitNativeSpawnNote+" "+rule) {
		t.Fatalf("the notice does not state the V2 rule after the CRW-1257 sentence: %q", out.Hook.Context)
	}
	if n := dispatchUTF16Len(out.Hook.Context); n > 4096 {
		t.Fatalf("the notice is %d UTF-16 units, over 4096", n)
	}
}
