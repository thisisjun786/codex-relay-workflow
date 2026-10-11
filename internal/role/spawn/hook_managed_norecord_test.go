package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1189 (S5-R3-F1): a marker whose dispatch record does not exist is refused with words that name the cause and the next action,
// and the oracle's own text (the ENOENT of the record's lstat) stays in the answer. The decision is unchanged: the spawn is denied.
func TestCRW1189MarkerWithoutADispatchRecordSaysWhatToDo(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnHookMust(t, os.MkdirAll(filepath.Join(rig.ws, ".crw", "dispatches", "rec-s1"), 0o755))
	stdin := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":"` + rig.ws + `","tool_use_id":"native-1","tool_input":{"agent_type":"executor","message":"[CRW-DISPATCH:none:att-9]\nTASK: implement"}}`
	got := RunSpawnAttachHook(stdin, rig.env)
	if !strings.Contains(got, `"permissionDecision":"deny"`) {
		t.Fatalf("a marker without a record must stay denied: %q", got)
	}
	for _, want := range []string{
		"managed dispatch: this marker has no dispatch record (ENOENT: no such file or directory, lstat ",
		"none.json",
		"`crw role helper dispatch`",
		"start",
		"claim",
		"explicitly asks for a direct native spawn",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal lacks %q: %q", want, got)
		}
	}
	// CRW-1189 round 2 (d1): the refusal never tells the agent to drop the marker on its own - that turns a refused managed attempt into
	// an unmanaged native spawn - and names the one case where a direct spawn is asked for: the user's own request.
	if strings.Contains(got, "or leave the marker out") || strings.Contains(got, "leave the marker out") {
		t.Errorf("the refusal advises dropping the marker unconditionally: %q", got)
	}
	if !strings.Contains(got, "user's current request") {
		t.Errorf("the direct spawn is not tied to the user's request: %q", got)
	}
}

// Other failures of a record keep the oracle's text alone.
func TestCRW1189OtherDispatchFailuresKeepTheOracleText(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	dir := filepath.Join(rig.ws, ".crw", "dispatches", "rec-s1")
	spawnHookMust(t, os.MkdirAll(dir, 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "one.json"), []byte(`{"version":1,"sessionId":"rec-s1","id":"one","role":"executor","candidates":[{"model":"rec/exec-primary","effort":"high"}],"attempts":[{"id":"att-1","candidate":{"model":"rec/exec-primary","effort":"high"},"claimed":false,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"ready","reconciliation":null,"spawnIssued":false,"toolUseId":null}],"status":"active"}`), 0o644))
	stdin := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":"` + rig.ws + `","tool_use_id":"native-1","tool_input":{"agent_type":"executor","message":"[CRW-DISPATCH:one:att-1]\nTASK: implement"}}`
	got := RunSpawnAttachHook(stdin, rig.env)
	if !strings.Contains(got, "managed dispatch: managed spawn attempt is not claimed or no longer current") || strings.Contains(got, "no dispatch record") {
		t.Errorf("an unclaimed attempt keeps its text: %q", got)
	}
}
